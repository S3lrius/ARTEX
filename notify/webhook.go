package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"
)

// webhookChannel is the generic webhook adapter: the user supplies the URL, method, headers, and JSON template.
// It exists so Slack, Mattermost, Discord, and a homegrown system do not each need their own implementation.
// A configurable template covers those platforms.
type webhookChannel struct{}

func (webhookChannel) Kind() string { return KindWebhook }

// A generic webhook has no official limit. Zero means no default rate limit, and the user sets one for the peer.
func (webhookChannel) DefaultRatePerMin() int { return 0 }

// url and headers are masked. The target address often carries a token, and custom headers usually hold auth credentials.
// Both show up in API responses, so both are blocked.
// The cost is that changing one header means re-entering the whole set (a masked value is treated as "keep the original").
// That tradeoff is deliberate: filling the form again is better than echoing a credential into the browser.
func (webhookChannel) SecretKeys() []string { return []string{"url", "headers"} }

// The destination is url. Changing url requires stating headers again. Otherwise the original Authorization header is sent to the new address, which is the main way around masking.
func (webhookChannel) DestinationKeys() []string { return []string{"url"} }

// webhookDefaultTemplate is the fallback body when no template is set: a plain JSON structure that covers most homegrown receivers that store one JSON document.
const webhookDefaultTemplate = `{
  "title": {{json .Title}},
  "batch": {{.Batch}},
  "count": {{.Count}},
  "items": [
{{- range $i, $it := .Items}}
{{- if $i}},{{end}}
    {
      "finding_id": {{$it.FindingID}},
      "name": {{json $it.Name}},
      "vulnclass": {{json $it.VulnClass}},
      "severity": {{json $it.Severity}},
      "summary": {{json $it.Summary}},
      "assets": {{json $it.Assets}},
      "detail_url": {{json $it.DetailURL}}
    }
{{- end}}
  ]
}`

// webhookTemplateData is the context exposed to the user's template.
type webhookTemplateData struct {
	Title   string
	Batch   bool
	Count   int
	Items   []webhookItem
	HomeURL string
	// SentAt is this delivery's time (RFC3339), for the receiver to record.
	SentAt string
}

type webhookItem struct {
	FindingID     int64
	Name          string
	VulnClass     string
	Severity      string
	SeverityLabel string
	Summary       string
	Assets        []string
	DetailURL     string
	FromStatus    string
	ToStatus      string
	// StatusLabel is a readable status-change description, such as "Pending → Fixed". It is empty when this is not a status change.
	StatusLabel string
}

func (webhookChannel) Validate(cfg map[string]any) error {
	raw := cfgString(cfg, "url")
	if raw == "" {
		return errors.New("target URL is missing")
	}
	if err := validateHTTPURL(raw); err != nil {
		return fmt.Errorf("target URL is invalid: %w", err)
	}
	if m := strings.ToUpper(cfgString(cfg, "method")); m != "" && m != http.MethodGet && m != http.MethodPost && m != http.MethodPut && m != http.MethodPatch {
		return fmt.Errorf("unsupported method %s (use GET, POST, PUT, or PATCH)", m)
	}
	if tpl := cfgString(cfg, "body_template"); tpl != "" {
		if _, err := parseWebhookTemplate(tpl); err != nil {
			return fmt.Errorf("body template syntax error: %w", err)
		}
	}
	return nil
}

func (c webhookChannel) Send(ctx context.Context, cfg map[string]any, m Message) (int, error) {
	if err := c.Validate(cfg); err != nil {
		return 0, Permanent(err)
	}
	method := strings.ToUpper(cfgString(cfg, "method"))
	if method == "" {
		method = http.MethodPost
	}

	// GET has no body. Putting the content in the query is beyond what the template can do, and it does not match GET.
	// GET only fits a receiver that fires a hook when something arrives.
	var payload any
	if method != http.MethodGet {
		body, err := renderWebhookBody(cfgString(cfg, "body_template"), m)
		if err != nil {
			return 0, Permanent(err)
		}
		// The template renders JSON as a string. It is sent as json.RawMessage so a second escape does not wrap the user's structure inside a JSON string.
		if !json.Valid([]byte(body)) {
			return 0, Permanent(errors.New("the rendered body template is not valid JSON"))
		}
		payload = json.RawMessage(body)
	}

	headers := cfgMap(cfg, "headers")
	if ct := cfgString(cfg, "content_type"); ct != "" {
		// An override is allowed, but it is applied after headers so an explicit setting wins.
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Content-Type"] = ct
	}
	if _, err := doJSON(ctx, method, cfgString(cfg, "url"), headers, payload); err != nil {
		return 0, err
	}
	// A generic webhook does not truncate the body (the receiver is the user's service, and the size comes from body_template), so the whole batch counts as delivered.
	return len(m.Items), nil
}

// renderWebhookBody renders the body with the user's template, or the default template.
func renderWebhookBody(tpl string, m Message) (string, error) {
	if strings.TrimSpace(tpl) == "" {
		tpl = webhookDefaultTemplate
	}
	t, err := parseWebhookTemplate(tpl)
	if err != nil {
		return "", fmt.Errorf("body template syntax error: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, newWebhookTemplateData(m)); err != nil {
		return "", fmt.Errorf("failed to render the body template: %w", err)
	}
	return buf.String(), nil
}

// parseWebhookTemplate parses a template.
//
// missingkey=zero renders a missing map key as the zero value instead of an error. The context here is a struct,
// so the main effect is that ranging over an empty .Items does not fail. The case that actually has to be guarded is a nil .Items.
func parseWebhookTemplate(tpl string) (*template.Template, error) {
	return template.New("body").Funcs(webhookTemplateFuncs).Option("missingkey=zero").Parse(tpl)
}

// webhookTemplateFuncs are the helpers exposed to the template.
var webhookTemplateFuncs = template.FuncMap{
	// json serializes any value as JSON.
	//
	// This function is required. Without it the user can only write {{.Title}} and interpolate directly.
	// A quote or a newline in a finding title then makes the whole body invalid JSON. The receiver rejects it,
	// and the error says the JSON failed to parse, with no hint that the title contained a quote.
	"json": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	},
	// jsons embeds a JSON fragment inside another JSON string value (one extra layer of string escaping).
	"jsons": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		quoted, err := json.Marshal(string(raw))
		if err != nil {
			return "", err
		}
		// Drop the outer quotes. The caller decides whether to add them.
		return string(quoted[1 : len(quoted)-1]), nil
	},
}

func newWebhookTemplateData(m Message) webhookTemplateData {
	d := webhookTemplateData{
		Title:   markdownTitle(m),
		Batch:   m.Batch,
		Count:   len(m.Items),
		HomeURL: m.HomeURL,
		SentAt:  time.Now().Format(time.RFC3339),
		Items:   make([]webhookItem, 0, len(m.Items)),
	}
	for _, it := range m.Items {
		wi := webhookItem{
			FindingID:     it.FindingID,
			Name:          it.Name,
			VulnClass:     it.VulnClass,
			Severity:      it.Severity,
			SeverityLabel: SeverityLabel(it.Severity),
			Summary:       it.Summary,
			Assets:        append([]string{}, it.Assets...),
			DetailURL:     it.DetailURL,
			FromStatus:    it.FromStatus,
			ToStatus:      it.ToStatus,
		}
		if it.IsStatusChange() {
			wi.StatusLabel = StatusLabel(it.FromStatus) + " → " + StatusLabel(it.ToStatus)
		}
		d.Items = append(d.Items, wi)
	}
	return d
}
