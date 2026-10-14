package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// assetInterceptCandidates extracts the domain/IP/URL candidate strings of one asset
// input waiting to be inserted, for asset-block matching. A URL's host is split out
// and classified, so a service or endpoint asset that carries only a URL can still
// hit a domain or IP rule.
func assetInterceptCandidates(item assetInputItem) (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, item.Domain)
	for _, d := range item.BoundDomains {
		add(&domains, d)
	}
	add(&ips, item.IP)
	add(&ips, item.ServiceIP)
	add(&urls, item.URL)
	if item.URL != "" {
		if u, err := url.Parse(item.URL); err == nil {
			if h := u.Hostname(); h != "" {
				if net.ParseIP(h) != nil {
					add(&ips, h)
				} else {
					add(&domains, h)
				}
			}
		}
	}
	return domains, ips, urls
}

// assetInputLabel returns a short label for one asset waiting to be inserted, used in block messages.
func assetInputLabel(item assetInputItem) string {
	typ := strings.TrimSpace(item.Type)
	var target string
	switch {
	case strings.TrimSpace(item.Domain) != "":
		target = strings.TrimSpace(item.Domain)
	case strings.TrimSpace(item.URL) != "":
		target = strings.TrimSpace(item.URL)
	case strings.TrimSpace(item.IP) != "":
		target = strings.TrimSpace(item.IP)
	case strings.TrimSpace(item.ServiceIP) != "":
		target = strings.TrimSpace(item.ServiceIP)
	default:
		target = "(unknown)"
	}
	if typ != "" {
		return fmt.Sprintf("[%s] %s", typ, target)
	}
	return target
}

// =====================================================================
// Unified asset insertion tools
// =====================================================================

// SetAssetStore wires the asset store and company store onto this ToolSet
// so the insert_assets, add_company_scope, and list_assets tools are active.
func (t *ToolSet) SetAssetStore(as *db.AssetStore, cs *db.CompanyStore) {
	t.as = as
	t.cs = cs
}

// assetInputItem is one element of the insert_assets "assets" array.
type assetInputItem struct {
	Type string `json:"type"` // root_domain|ip|subdomain|app|service|endpoint

	// ---- root_domain / subdomain ----
	Domain      string   `json:"domain"`
	ICP         string   `json:"icp"`
	RecordType  string   `json:"record_type"`
	RecordValue []string `json:"record_value"`

	// ---- ip ----
	IP           string           `json:"ip"`
	BoundDomains []string         `json:"bound_domains"`
	OpenPorts    []db.PortService `json:"open_ports"`

	// ---- app ----
	AppName     string `json:"app_name"`
	BundleID    string `json:"bundle_id"`
	Category    string `json:"category"`
	Description string `json:"description"`
	AppICP      string `json:"app_icp"`
	CompanyID   *int64 `json:"company_id"` // explicit company link (app only; others auto-attribute via scope)

	// ---- service (http) ----
	URL           string           `json:"url"`
	Technologies  []string         `json:"technologies"`
	StatusCode    *int             `json:"status_code"`
	ContentLength *int64           `json:"content_length"`
	PageTitle     string           `json:"page_title"`
	FaviconMMH3   string           `json:"favicon_mmh3"`
	Auth          []map[string]any `json:"auth"`
	ServiceName   string           `json:"service_name"`
	ServiceIP     string           `json:"service_ip"` // optional enrichment IP

	// ---- service (other) ----
	Port  int    `json:"port"`
	Proto string `json:"proto"`

	// ---- endpoint ----
	Method string           `json:"method"`
	Params []map[string]any `json:"params"`
}

// insertAssets is the unified insert_assets agent tool.
func (t *ToolSet) insertAssets() actool.CoreTool {
	return writeTool(
		"insert_assets",
		"Register newly found assets in a batch. One call may mix several types (type is the enum).\n"+
			"Required fields by type: root_domain→domain; ip→ip (must be IPv4/IPv6, not a hostname); subdomain→domain; app→app_name; service(HTTP)→url; service(non-HTTP)→service_name+port (at least one of ip/domain); endpoint→url+method. Other fields are explained on each one.\n"+
			"auth/technologies/params are appended, and do not overwrite the previous value.\n"+
			"Returns: {results:[{index,id,type}], errors:[{index,error}]}",
		obj(map[string]any{
			// task_id is not exposed to the model: which task a worker belongs to is set authoritatively by the program through SetTaskID (see the handler).
			"assets": map[string]any{
				"type":        "array",
				"description": "asset array; each element is one asset record",
				"items": obj(map[string]any{
					"type": map[string]any{
						"type":        "string",
						"enum":        []string{"root_domain", "ip", "subdomain", "app", "service", "endpoint"},
						"description": "asset type",
					},
					// root_domain / subdomain
					"domain":      str("root domain or subdomain (required for root_domain/subdomain)"),
					"icp":         str("ICP filing number (optional)"),
					"record_type": str("DNS record type: A/AAAA/CNAME/MX and so on (optional for subdomain)"),
					"record_value": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "DNS record values (optional for subdomain, for example [\"1.2.3.4\",\"2.3.4.5\"])",
					},
					// ip
					"ip": str("IP address. Must be an IPv4/IPv6 address, not a hostname (a hostname uses the domain field of type=subdomain). Required for type ip. Optional for service/endpoint, used to associate the IP"),
					"bound_domains": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "domains bound to this IP (optional for type ip)",
					},
					"open_ports": map[string]any{
						"type":        "array",
						"description": "open ports (optional for type ip)",
						"items": obj(map[string]any{
							"port":    intp("port number"),
							"service": str("service name, such as http/ssh/mysql (optional)"),
						}, "port"),
					},
					// app
					"app_name":    str("application name (required for type app)"),
					"bundle_id":   str("Bundle ID (optional for type app)"),
					"category":    str("application category (optional)"),
					"description": str("application description (optional)"),
					"app_icp":     str("application ICP filing (optional)"),
					"company_id":  intp("owning company id (optional for type app; an app cannot be attributed automatically from scope and must be set explicitly. The id is returned by add_company_scope)"),
					// service (http)
					"url":         str("full URL, including scheme and port (required for an HTTP service; service_type is set to http automatically)"),
					"status_code": intp("HTTP response status code, such as 200/301/403/404 (optional)"),
					"content_length": map[string]any{
						"type":        "integer",
						"description": "HTTP response body size in bytes (optional)",
					},
					"page_title":   str("page <title> text (optional)"),
					"favicon_mmh3": str("favicon MMH3 hash (optional)"),
					"technologies": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "fingerprint / technology list, for example [\"Nginx\",\"Vue\",\"Bootstrap\"] (optional)",
					},
					"auth": map[string]any{
						"type":        "array",
						"description": "authentication records found, each with fields such as type/username/password (optional, appended and not overwritten)",
						"items":       map[string]any{"type": "object"},
					},
					// service (other, non-HTTP)
					"service_name": str("service name, such as ssh/mysql/redis (required when service is not HTTP)"),
					"port":         intp("port number (required when service is not HTTP)"),
					// endpoint
					"method": str("HTTP method: GET/POST/PUT/PATCH/DELETE and so on (required for endpoint)"),
					"params": map[string]any{
						"type":        "array",
						"description": "request parameters, each with location (query/body/header/path)/name/value/type (optional, appended and not overwritten)",
						"items":       map[string]any{"type": "object"},
					},
				}, "type"),
			},
		}, "assets"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("insert_assets is not enabled: AssetStore is not initialized"), nil
			}
			var a struct {
				Assets []assetInputItem `json:"assets"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf("invalid input: " + err.Error()), nil
			}
			// task_id is set authoritatively by the program (worker: SetTaskID) and is not accepted from the model, so a missing or wrong value from the model cannot leave an asset unassigned or assigned to the wrong task. Callers with no task context (auto/pentest/chat) have t.taskID=0.
			taskID := t.taskID

			type result struct {
				Index int    `json:"index"`
				ID    int64  `json:"id"`
				Type  string `json:"type"`
			}
			type errEntry struct {
				Index int    `json:"index"`
				Error string `json:"error"`
			}

			var results []result
			var errs []errEntry

			// Asset-gate rules are loaded once. If the read fails, skip the judgment (do not block the insert).
			// Block rules = global union task-level block; allow rules = task-level allow.
			blockRules, _ := t.as.ListAssetInterceptRules()
			var allowRules []db.AssetInterceptRule
			if t.taskID > 0 {
				if tb, ta, err := t.as.TaskInterceptRulesSplit(t.taskID); err == nil {
					blockRules = append(blockRules, tb...)
					allowRules = ta
				}
			}

			for i, item := range a.Assets {
				// Asset gate: block first, then allow. A rejected asset must not be inserted (skip Upsert and the later side effects).
				domains, ips, urls := assetInterceptCandidates(item)
				if d := db.EvaluateAssetGate(blockRules, allowRules, domains, ips, urls); !d.Allowed {
					errs = append(errs, errEntry{
						Index: i,
						Error: fmt.Sprintf("asset %s %s, insert forbidden", assetInputLabel(item), d.Reason),
					})
					continue
				}

				typ := strings.TrimSpace(item.Type)
				var id int64
				var err error

				switch typ {
				case "root_domain":
					id, err = t.as.UpsertRootDomain(db.UpsertRootDomainReq{
						Domain: item.Domain,
						ICP:    item.ICP,
						TaskID: taskID,
					})

				case "ip":
					id, err = t.as.UpsertIP(db.UpsertIPReq{
						IP:           item.IP,
						BoundDomains: item.BoundDomains,
						OpenPorts:    item.OpenPorts,
						TaskID:       taskID,
					})

				case "subdomain":
					id, err = t.as.UpsertSubdomain(db.UpsertSubdomainReq{
						Domain:      item.Domain,
						RecordType:  item.RecordType,
						RecordValue: item.RecordValue,
						ICP:         item.ICP,
						TaskID:      taskID,
					})

				case "app":
					id, err = t.as.UpsertApp(db.UpsertAppReq{
						Name:        item.AppName,
						BundleID:    item.BundleID,
						Category:    item.Category,
						Description: item.Description,
						ICP:         item.AppICP,
						CompanyID:   item.CompanyID,
						TaskID:      taskID,
					})

				case "service":
					// distinguish HTTP vs other by presence of url
					if item.URL != "" {
						// agent may send "ip" or "service_ip" for the enrichment IP; accept both
						svcIP := item.ServiceIP
						if svcIP == "" {
							svcIP = item.IP
						}
						id, err = t.as.UpsertHTTPService(db.UpsertHTTPServiceReq{
							URL:           item.URL,
							Technologies:  item.Technologies,
							StatusCode:    item.StatusCode,
							ContentLength: item.ContentLength,
							PageTitle:     item.PageTitle,
							FaviconMMH3:   item.FaviconMMH3,
							Auth:          item.Auth,
							IP:            svcIP,
							TaskID:        taskID,
						})
					} else {
						id, err = t.as.UpsertOtherService(db.UpsertOtherServiceReq{
							Domain:      item.Domain,
							IP:          item.IP,
							Port:        item.Port,
							ServiceName: item.ServiceName,
							Auth:        item.Auth,
							TaskID:      taskID,
						})
					}

				case "endpoint":
					id, err = t.as.UpsertEndpoint(db.UpsertEndpointReq{
						URL:    item.URL,
						Method: item.Method,
						Params: item.Params,
						IP:     item.ServiceIP,
						TaskID: taskID,
					})

				default:
					errs = append(errs, errEntry{Index: i, Error: "unknown type: " + typ})
					continue
				}

				if err != nil {
					errs = append(errs, errEntry{Index: i, Error: err.Error()})
					continue
				}
				results = append(results, result{Index: i, ID: id, Type: typ})
				t.writes.Assets++
				t.anchorOwner(id)
				if taskID > 0 {
					var sourceNodeID *int64
					if t.ownerNode > 0 {
						nodeID := t.ownerNode
						sourceNodeID = &nodeID
					}
					summary := "Agent registered it through insert_assets"
					if t.ownerNode > 0 {
						summary = fmt.Sprintf("Worker intent #%d registered it through insert_assets", t.ownerNode)
					}
					_ = t.as.SetTaskAssetSource(taskID, id, "agent", summary, sourceNodeID)
				}
				// Automatic test scope (source='auto'): only this item the worker explicitly inserted at the top level, and a conservative scope for its type. Assets derived as side effects do not pass through here, so the scope is not widened blindly. No-op when taskID=0.
				// Independent of the coverage switch: task_scope is the task's range boundary (the filter basis for list/query). The coverage switch only decides whether to use it as the denominator for a metric, not whether to accumulate the scope itself.
				{
					svcIP := item.ServiceIP
					if svcIP == "" {
						svcIP = item.IP
					}
					_ = t.as.AddAutoScope(taskID, typ, item.Domain, item.URL, svcIP)
				}
			}

			return jsonResult(map[string]any{
				"results": results,
				"errors":  errs,
			})
		},
	)
}

// addCompanyScope writes to company_scope table and triggers asset attribution.
func (t *ToolSet) addCompanyScope() actool.CoreTool {
	return writeTool(
		"add_company_scope",
		"Add a domain, IP, CIDR, ICP filing, or company keyword to a company's asset scope. Domains, networks, and ICP automatically claim matching assets. A keyword is only a scope hint for the agent.\n"+
			"Company names are unique: if company does not exist it is created; if it exists it is reused (only the scope is merged in).\n"+
			"scope is one entry per line. The system classifies each line: root domain / URL / single IP / CIDR network / ICP filing / company keyword.\n"+
			"Always give reason for the ownership basis (whois, certificate, ASN, and so on).\n"+
			"Guardrails: a bare TLD and an overly wide network are rejected (an IPv4 prefix must be /16-/32, an IPv6 prefix /32-/128). Illegal lines are skipped and returned in errors.",
		obj(map[string]any{
			"company": str("company name (created if missing, reused if it exists; the name is unique)"),
			"scope":   str("asset scope, one entry per line: domain / URL / IP / CIDR / ICP filing / company keyword"),
			"reason":  str("ownership basis (evidence/source); required"),
			"logo":    str("company icon URL (optional; applied only when the company is created)"),
		}, "company", "scope"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("add_company_scope is not enabled: CompanyStore is not initialized"), nil
			}
			var a struct {
				Company string `json:"company"`
				Scope   string `json:"scope"`
				Reason  string `json:"reason"`
				Logo    string `json:"logo"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if strings.TrimSpace(a.Company) == "" {
				return actool.Errorf("company must not be empty"), nil
			}
			companyID, _, err := t.cs.UpsertCompany(a.Company, a.Logo)
			if err != nil {
				return actool.Errorf("failed to create or get the company: " + err.Error()), nil
			}
			lines := splitLines(a.Scope)
			added, skipped, invalid, errMsgs := t.cs.AddScope(companyID, lines, a.Reason)
			out := map[string]any{
				"company_id": companyID,
				"added":      added,
				"skipped":    skipped,
				"invalid":    invalid,
			}
			if len(errMsgs) > 0 {
				out["errors"] = errMsgs
			}
			return jsonResult(out)
		},
	)
}

// addTaskScope lets the plan agent add test scope to THE CURRENT TASK — the coverage
// denominator and the task's authorization edge. Worker discoveries are auto-scoped
// (precise host) by insertAssets; this tool is for DELIBERATELY WIDENING: pull a whole
// root domain or whole company into scope, or add a specific subdomain / ip.
func (t *ToolSet) addTaskScope() actool.CoreTool {
	return writeTool(
		"add_task_scope",
		"Add test scope to this task. This is the task's authorization boundary, and the denominator for asset test coverage.\n"+
			"kind may be: company (every asset under the company) / root_domain (the whole root domain, including every subdomain) / subdomain (one exact subdomain) / ip / cidr / icp / keyword.\n"+
			"Note: a host a worker encounters one by one is added to scope automatically (exact subdomain). This tool is for widening on purpose — bring in a whole root domain or a whole company, or add a specific subdomain or IP.\n"+
			"value: company takes a company name or id (the company must already exist); root_domain/subdomain take a domain; ip/cidr take an IP or network; icp/keyword take a filing number or a company keyword.\n"+
			"Always give reason for the basis (it can be audited). Use the entries array for several.",
		obj(map[string]any{
			"entries": map[string]any{"type": "array", "description": "Batch: [{kind, value}]. kind is one of company/root_domain/subdomain/ip/cidr/icp/keyword.", "items": map[string]any{"type": "object"}},
			"kind":    str("[single] company / root_domain / subdomain / ip / cidr / icp / keyword"),
			"value":   str("[single] company name or id / domain / IP / CIDR / ICP / keyword"),
			"reason":  str("basis for adding it (for audit); required"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("add_task_scope is not enabled: AssetStore is not initialized"), nil
			}
			if t.taskID <= 0 {
				return actool.Errorf("add_task_scope needs a task context (there is no task)"), nil
			}
			type scopeEntry struct {
				Kind  string `json:"kind"`
				Value string `json:"value"`
			}
			var a struct {
				Entries    []scopeEntry `json:"entries"`
				scopeEntry              // single mode
				Reason     string       `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			items := a.Entries
			if len(items) == 0 {
				items = []scopeEntry{a.scopeEntry}
			}
			var added []map[string]any
			errs := map[string]string{}
			for i, e := range items {
				ts, err := t.as.AddAgentScope(t.taskID, strings.TrimSpace(e.Kind), e.Value, a.Reason, "agent")
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				added = append(added, map[string]any{"kind": ts.Kind, "domain": ts.Domain, "net": ts.Net, "value": ts.Value, "company_id": ts.CompanyID})
			}
			out := map[string]any{"added": added}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		},
	)
}

// listUntestedAssets lets the plan agent pull the current + directly inherited
// scope's not-yet-tested assets on demand (filter by type, paginated).
func (t *ToolSet) listUntestedAssets() actool.CoreTool {
	return readTool(
		"list_untested_assets",
		"Query assets inside the scope of this task and directly related tasks that are not yet covered by a fact anchor (related scope is read-only; it is for you to judge whether to test more, and it does not decide for you).\n"+
			"Optional filter by asset type: root_domain/subdomain/service/app/endpoint/ip.\n"+
			"Paging: page starts at 1, page_size defaults to 10. Returns {assets:[{id,type,label}], total, page, page_size}. Available only in a task context.",
		obj(map[string]any{
			"type":      str("asset type filter (optional): root_domain/subdomain/service/app/endpoint/ip"),
			"page":      intp("page number, starting at 1 (default 1)"),
			"page_size": intp("page size (default 10)"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_untested_assets is not enabled: AssetStore is not initialized"), nil
			}
			if t.taskID <= 0 || t.ts == nil {
				return actool.Errorf("list_untested_assets needs a task context"), nil
			}
			var a struct {
				Type     string `json:"type"`
				Page     int    `json:"page"`
				PageSize int    `json:"page_size"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Page <= 0 {
				a.Page = 1
			}
			if a.PageSize <= 0 {
				a.PageSize = 10
			}
			offset := (a.Page - 1) * a.PageSize
			assets, total, err := t.as.ListUntestedAssetsWithSources(t.taskID, strings.TrimSpace(a.Type), a.PageSize, offset)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(map[string]any{
				"assets": assets, "total": total, "page": a.Page, "page_size": a.PageSize,
			})
		},
	)
}

// listAssets lets an agent query the asset table.
func (t *ToolSet) listAssets() actool.CoreTool {
	return readTool(
		"list_assets",
		"Query the asset store: search with a DSL expression, or fetch directly by id/ids. Paging is supported. Only assets inside the test scope of this task and directly related tasks are returned.\n"+
			"DSL: field=value is fuzzy (ILIKE) | field==value is exact | field!=value excludes | numeric fields support > >= < <= | a bare word is full-text fuzzy. AND/OR combine them (AND has higher precedence), and parentheses may group. Asset type uses the separate type parameter and is not written into the DSL.\n"+
			"When id/ids are not passed, dsl must be non-empty (an unconditional full query is not allowed).\n"+
			"Fields: domain (root/sub/service domain), root_domain, ip, url, page_title, icp, service_name, app_name, method (such as GET/POST), service_type (http|other), record_type (such as A/CNAME), technology (array; = is fuzzy, == is exact), port/status_code/company_id (integers).\n"+
			"Example: status_code>=400 AND technology=shiro ; (port==80 OR port==443) AND technology=nginx",
		obj(map[string]any{
			"dsl":    str(`DSL query expression (syntax and fields are in the tool description). Must be non-empty when id/ids are not passed.`),
			"type":   str("asset type filter: root_domain|ip|subdomain|app|service|endpoint (a separate field; it can be combined with dsl; type alone is not enough to query, dsl is still required)"),
			"id":     intp("fetch one asset id directly (optional, mutually exclusive with dsl/type)"),
			"ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "fetch several asset ids directly (optional, mutually exclusive with dsl/type)"},
			"limit":  intp("return cap, default 10 (optional)"),
			"offset": intp("page offset, default 0 (optional)"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_assets is not enabled: AssetStore is not initialized"), nil
			}
			var a struct {
				DSL    string  `json:"dsl"`
				Type   string  `json:"type"`
				ID     int64   `json:"id"`
				IDs    []int64 `json:"ids"`
				Limit  int     `json:"limit"`
				Offset int     `json:"offset"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Limit <= 0 {
				a.Limit = 10
			}

			var assets []*db.Asset
			var err error
			switch {
			case a.ID > 0:
				assets, err = t.as.GetByIDsInScope(t.taskID, []int64{a.ID})
			case len(a.IDs) > 0:
				assets, err = t.as.GetByIDsInScope(t.taskID, a.IDs)
			case a.DSL != "":
				assets, err = t.as.QueryDSLInScope(a.DSL, a.Type, t.taskID, a.Limit, a.Offset)
			default:
				return actool.Errorf("dsl must not be empty when id/ids are not passed: querying every asset with no condition is not allowed, so provide a query"), nil
			}
			if err != nil {
				return actool.Errorf("DSL error: " + err.Error()), nil
			}
			return jsonResult(map[string]any{
				"count":  len(assets),
				"assets": assets,
			})
		},
	)
}

// listCompanies lets an agent enumerate companies with their scope + asset count.
func (t *ToolSet) listCompanies() actool.CoreTool {
	return readTool(
		"list_companies",
		"List companies in the asset store, with their asset scope and the count of assets already attributed. Use it to see which companies exist and "+
			"to get company_id (used when insert_assets links an app, and when list_assets filters by company_id). "+
			"Optional search fuzzy-filters by company name (case-insensitive); empty returns all.",
		obj(map[string]any{
			"search": str("fuzzy filter by company name (optional, case-insensitive); empty returns all"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("list_companies is not enabled: CompanyStore is not initialized"), nil
			}
			var a struct {
				Search string `json:"search"`
			}
			_ = json.Unmarshal(in, &a)
			cos, err := t.cs.ListCompanies()
			if err != nil {
				return actool.Errorf("failed to query companies: " + err.Error()), nil
			}
			q := strings.ToLower(strings.TrimSpace(a.Search))
			type companyOut struct {
				ID         int64    `json:"id"`
				Name       string   `json:"name"`
				AssetCount int      `json:"asset_count"`
				Scope      []string `json:"scope"`
			}
			out := make([]companyOut, 0, len(cos))
			for _, c := range cos {
				if q != "" && !strings.Contains(strings.ToLower(c.Name), q) {
					continue
				}
				scope := make([]string, 0, len(c.Scope))
				for _, r := range c.Scope {
					scope = append(scope, r.Raw)
				}
				out = append(out, companyOut{ID: c.ID, Name: c.Name, AssetCount: c.AssetCount, Scope: scope})
			}
			return jsonResult(map[string]any{"count": len(out), "companies": out})
		},
	)
}

// splitLines splits a multi-line string into non-empty trimmed lines.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// WorkerTools returns the tool set for a work agent.
func (t *ToolSet) WorkerTools() []actool.CoreTool {
	return []actool.CoreTool{
		// list_findings stays: before reporting a vulnerability, check this task's confirmed vulnerabilities so the same one is not reported twice.
		t.listFindings(),
		t.addFinding(), t.recordFact(),
		// asset management (handlers guard a nil store internally).
		// add_company_scope is not given to the worker: defining a company's asset scope belongs to planning, the main agent, or Auto. The worker only executes exploration.
		t.insertAssets(), t.listAssets(),
		// Looking back across works: a worker may also reuse another work's observations and avoid repeating the work.
		// search_all_worker_traces: no need to know intent_id first; pull matching steps globally by keyword.
		// get_worker_trace: once a work is pinned, list its steps, search in place, or fetch full content.
		t.searchAllWorkerTraces(), t.getWorkerTrace(),
		// node_detail: once the worker has an intent_id or node id, it can read that node's full detail (together with the look-back above).
		t.nodeDetail(),
		// These tools are still not given to the worker and are left to the planner/main (reading context and reviewing across works is a planning duty;
		// the worker only executes and writes back its one intent): list_facts / list_companies / list_worker_traces.
	}
}

// MainAgentTools returns the human-interface tool set.
func (t *ToolSet) MainAgentTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		t.expandDigest(), // cold-digest §6.1
		t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(), t.addHint(), t.addIntent(),
		// steer_work: the human can inject a real-time course correction into a running intent (work) (it does not interrupt and does not drop progress).
		t.steerWorkTool(),
		// set_goals: the human can add a new final goal to this task at runtime (the planner re-judges whether it is met from that).
		t.setGoals(),
		// set_constraints: the human can add or change operation constraints (allow/deny) for this task at runtime, bounding the planner/worker's exploration.
		t.setConstraints(),
		// asset management (handlers guard nil store internally)
		t.insertAssets(), t.addCompanyScope(), t.listAssets(),
		t.addFinding(), t.recordFact(),
		t.addTaskScope(),
		// list_untested_assets: when needed, list untested assets inside this task's scope (type plus paging) and decide for yourself whether to test more.
		t.listUntestedAssets(),
	}
}

// AllDomainTools returns the union of all domain tools across all agent types,
// deduped by name (mainagent order wins). Used by the server to build a registry
// for injecting domain tools into agents (Auto, custom) that don't own a per-task
// ToolSet. The caller provides real stores; tools are callable at taskID=0 scope.
func (t *ToolSet) AllDomainTools() []actool.CoreTool {
	seen := map[string]bool{}
	var out []actool.CoreTool
	all := append(append(t.MainAgentTools(), t.PlannerTools()...), t.WorkerTools()...)
	for _, tool := range all {
		if !seen[tool.Name()] {
			seen[tool.Name()] = true
			out = append(out, tool)
		}
	}
	return out
}
