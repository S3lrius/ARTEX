package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	actool "github.com/Autumn-27/norma/tool"
)

const accessDiffDescription = "Authenticated access-control differential test for IDOR/BOLA (the class that pays on mature programs). " +
	"Give a URL that belongs to the OWNER identity and name a SECOND identity to test. The tool fetches the URL three ways — as the owner (baseline), as the second identity, and unauthenticated — through the recording proxy, which injects each session's real credential server-side (you never handle credentials). It then checks whether the OWNER's canary values leak into the other responses: an owner canary in the second identity's response = cross-tenant IDOR; in the unauthenticated response = auth bypass. " +
	"Use it on harvested object references the owner can access (/orders/{id}, /account/{id}, export/report endpoints, GraphQL node ids) to prove another identity can read them. The three requests are recorded (credentials redacted) so you can cite them as evidence in report_finding. " +
	"Requires traffic capture ON and at least two identities configured, with canaries on the owner (owner-unique strings such as its account email or an order id)."

func accessDiffSchema() map[string]any {
	return objSchema(map[string]any{
		"url":    strParam("full URL of an object owned by the owner identity, e.g. https://manager.example.com/api/orders/123"),
		"owner":  strParam("identity name that OWNS this URL's resource (its canaries are matched in the other responses)"),
		"test":   strParam("identity name to test for unauthorized access to the owner's resource"),
		"method": strParam("optional HTTP method (default GET)"),
	}, "url", "owner", "test")
}

// toolAccessDiff is the credentialed IDOR/BOLA primitive. It issues the same
// request as owner / as the test identity / unauthenticated THROUGH the recording
// proxy (which injects each identity's real headers on host-match and redacts them
// from the stored traffic), then flags a leak when the owner's canaries surface in
// another identity's response. Bound to worker + deepdive (see seedOrchestrationTools).
func (s *Server) toolAccessDiff() actool.CoreTool {
	return wrTool("access_diff", accessDiffDescription, accessDiffSchema(),
		func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
			var a struct {
				URL    string `json:"url"`
				Owner  string `json:"owner"`
				Test   string `json:"test"`
				Method string `json:"method"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			a.URL, a.Owner, a.Test = strings.TrimSpace(a.URL), strings.TrimSpace(a.Owner), strings.TrimSpace(a.Test)
			if a.URL == "" || a.Owner == "" || a.Test == "" {
				return actool.Errorf("url, owner and test are required"), nil
			}
			if a.Method == "" {
				a.Method = http.MethodGet
			}
			tr := s.httpProxyTransport(httpExec{UseRecordingProxy: true})
			if tr == nil {
				return actool.Errorf("access_diff needs traffic capture ON — the recording proxy is what injects the identities' credentials"), nil
			}
			ids, err := s.m.pg.ListIdentities()
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			var ownerCanaries []string
			ownerFound, testFound := false, false
			for _, id := range ids {
				if strings.EqualFold(id.Name, a.Owner) {
					ownerCanaries, ownerFound = id.Canaries, true
				}
				if strings.EqualFold(id.Name, a.Test) {
					testFound = true
				}
			}
			if !ownerFound {
				return actool.Errorf("unknown owner identity: " + a.Owner), nil
			}
			if !testFound {
				return actool.Errorf("unknown test identity: " + a.Test), nil
			}
			if len(ownerCanaries) == 0 {
				return actool.Errorf("owner identity '" + a.Owner + "' has no canaries; add owner-unique strings (its account email, an order id it owns) so an IDOR can be proven by content, not just a status code"), nil
			}
			client := &http.Client{
				Transport: tr,
				Timeout:   30 * time.Second,
				// Each redirect hop would be its own proxy-injected request; for a clean
				// access check we read the direct response rather than auto-following.
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			}
			fetch := func(identity string) (int, string, error) {
				req, err := http.NewRequestWithContext(ctx, a.Method, a.URL, nil)
				if err != nil {
					return 0, "", err
				}
				if identity != "" {
					req.Header.Set("X-Artex-Identity", identity) // proxy swaps for the real session, then strips this
				}
				resp, err := client.Do(req)
				if err != nil {
					return 0, "", err
				}
				defer resp.Body.Close()
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
				return resp.StatusCode, string(body), nil
			}
			ownerStatus, _, err := fetch(a.Owner)
			if err != nil {
				return actool.Errorf("owner request failed: " + err.Error()), nil
			}
			testStatus, testBody, err := fetch(a.Test)
			if err != nil {
				return actool.Errorf("test request failed: " + err.Error()), nil
			}
			unauthStatus, unauthBody, uErr := fetch("")
			if uErr != nil {
				unauthStatus = -1 // a failed unauth request is not fatal to the comparison
			}
			leaked := func(body string) []string {
				lb := strings.ToLower(body)
				var hit []string
				for _, c := range ownerCanaries {
					if c != "" && strings.Contains(lb, strings.ToLower(c)) {
						hit = append(hit, c)
					}
				}
				return hit
			}
			leakToTest, leakToUnauth := leaked(testBody), leaked(unauthBody)
			verdict := "no_leak"
			note := "Owner's canaries did not appear in the other responses; access appears properly controlled."
			switch {
			case len(leakToTest) > 0:
				verdict = "idor"
				note = "IDOR/BOLA: identity '" + a.Test + "' received the owner's resource (owner canary present in its response). Verify independently, then report_finding citing the owner/test/unauth requests as evidence."
			case len(leakToUnauth) > 0:
				verdict = "auth_bypass"
				note = "Auth bypass: the unauthenticated request received the owner's resource. Verify and report."
			}
			return jsonResult(map[string]any{
				"url":                      a.URL,
				"verdict":                  verdict,
				"owner_status":             ownerStatus,
				"test_status":              testStatus,
				"unauth_status":            unauthStatus,
				"owner_canaries_in_test":   leakToTest,
				"owner_canaries_in_unauth": leakToUnauth,
				"note":                     note,
			})
		})
}
