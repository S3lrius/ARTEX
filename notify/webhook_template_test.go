package notify

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// This file locks the capability boundary of the generic webhook template.
//
// This is the only place in the package where a user-supplied string is evaluated as code, so what it can and
// cannot do has to be explicit, and tests pin those properties down. Otherwise someone can add a method to the
// template context, or a readFile to the FuncMap, and the capability grows silently while the diff looks like a harmless little function.

// TestTemplateContextHasNoMethods is the most important case.
//
// text/template calls exported methods ({{.Foo}} can be a field or a method). If the template context can reach
// any type with an exported method, those methods are exposed to the template author.
// This context is deliberately pure data: exported fields only, and no methods.
//
// If this fails, someone added a method to webhookTemplateData or webhookItem.
// Before allowing that, decide whether the template could use the method to read something that should stay hidden.
func TestTemplateContextHasNoMethods(t *testing.T) {
	for _, v := range []any{webhookTemplateData{}, webhookItem{}} {
		typ := reflect.TypeOf(v)
		if n := typ.NumMethod(); n != 0 {
			var names []string
			for i := 0; i < n; i++ {
				names = append(names, typ.Method(i).Name)
			}
			t.Fatalf("%s exposes %d methods (%s): text/template can call them, "+
				"which opens those methods to the template author", typ.Name(), n, strings.Join(names, ", "))
		}
	}
}

// TestTemplateFuncsAreMinimal locks the set of functions exposed to the template.
//
// Every extra function in the FuncMap is an extra capability. Today there are only json and jsons, which serialize a value into a JSON fragment. They cannot read files, send requests, or run commands.
func TestTemplateFuncsAreMinimal(t *testing.T) {
	var got []string
	for name := range webhookTemplateFuncs {
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"json", "jsons"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the template function set changed: got %v, expected %v. Before adding a function, confirm it does not widen the capability "+
			"(no file access, no network requests, no command execution)", got, want)
	}
}

// TestTemplateCannotReachUnknownData covers an out-of-range access in a template.
// Reaching something that does not exist must fail, not echo a value, and the failure must not leak internal data.
func TestTemplateCannotReachUnknownData(t *testing.T) {
	_, err := renderWebhookBody(`{"x": {{.Environment}}, "y": {{.Env}}}`, singleMsg())
	if err == nil {
		t.Fatal("accessing a field that does not exist should fail")
	}
	// The error must not contain real content from the template context (the finding title or summary).
	for _, leak := range []string{"SQL injection", "parameter id"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the template error leaked message content %q: %v", leak, err)
		}
	}
}

// TestTemplateRenderFailsPermanently: a bad template is a configuration error, and a retry will not fix it.
// If it were retryable, one bad template would burn three backoff rounds on every delivery.
func TestTemplateRenderFailsPermanently(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://example.com/hook",
		"body_template": `{{.Items.`,
	}
	if err := (webhookChannel{}).Validate(cfg); err == nil {
		t.Fatal("a template syntax error should be rejected when the config is saved")
	}
	// Even if validation is bypassed and delivery is attempted, it must be a permanent failure, not a retry loop.
	_, err := (webhookChannel{}).Send(context.Background(), cfg, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("a bad template should be a permanent failure, got %v", err)
	}
}

// TestTemplateCanOnlyProduceJSON covers the constraint that a rendered template must be valid JSON.
// It also blocks using the template to emit plain text that triggers some other protocol.
func TestTemplateCanOnlyProduceJSON(t *testing.T) {
	// A valid template passes.
	ok := map[string]any{"url": "https://example.com/hook", "body_template": `{"t":{{json .Title}}}`}
	if err := (webhookChannel{}).Validate(ok); err != nil {
		t.Fatalf("a valid template should pass validation: %v", err)
	}
	// A render that is not JSON must be refused, not sent as-is.
	bad := map[string]any{"url": "http://127.0.0.1:1/hook", "body_template": `not json {{.Count}}`}
	_, err := (webhookChannel{}).Send(context.Background(), bad, singleMsg())
	if err == nil || !IsPermanent(err) {
		t.Fatalf("a render that is not JSON should be a permanent failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "valid JSON") {
		t.Errorf("the error should say this is a JSON problem, got %v", err)
	}
}
