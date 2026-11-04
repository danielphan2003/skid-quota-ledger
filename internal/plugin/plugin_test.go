package plugin_test

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Silthus/cliproxy-quota-ledger/internal/plugin"
)

const samplesPath = "/v0/management/quota-ledger/samples"

var now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type httpResponse struct {
	StatusCode int
	Headers    map[string][]string
	Body       []byte
}

func call(t *testing.T, p *plugin.Plugin, method string, request any) envelope {
	t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	reply, ok := p.Call(method, raw)
	var decoded envelope
	if err := json.Unmarshal(reply, &decoded); err != nil {
		t.Fatalf("%s returned invalid envelope %q: %v", method, reply, err)
	}
	if decoded.OK != ok {
		t.Fatalf("%s returned ok=%v with envelope ok=%v", method, ok, decoded.OK)
	}
	return decoded
}

func register(t *testing.T, p *plugin.Plugin, method, configYAML string) envelope {
	t.Helper()
	return call(t, p, method, map[string]any{"config_yaml": []byte(configYAML), "schema_version": 7})
}

func registeredPlugin(t *testing.T) (*plugin.Plugin, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger", "samples.jsonl")
	p := plugin.New(func() time.Time { return now })
	if reply := register(t, p, "plugin.register", "enabled: true\npriority: 0\npath: "+path+"\n"); !reply.OK {
		t.Fatalf("plugin.register failed: %+v", reply.Error)
	}
	return p, path
}

func manage(t *testing.T, p *plugin.Plugin, method string, query url.Values, body string) httpResponse {
	t.Helper()
	request := map[string]any{
		"Method":           method,
		"Path":             samplesPath,
		"Headers":          map[string][]string{"Authorization": {"Bearer redacted"}},
		"Query":            query,
		"Body":             []byte(body),
		"host_callback_id": "callback-1",
	}
	reply := call(t, p, "management.handle", request)
	if !reply.OK {
		t.Fatalf("management.handle failed: %+v", reply.Error)
	}
	var response httpResponse
	if err := json.Unmarshal(reply.Result, &response); err != nil {
		t.Fatal(err)
	}
	if got := response.Headers["Content-Type"]; len(got) != 1 || got[0] != "application/json" {
		t.Fatalf("Content-Type = %v, want application/json", got)
	}
	return response
}

func sampleJSON(at time.Time, remaining float64) string {
	return fmt.Sprintf(`{"at":%d,"auth":"codex-a.json","provider":"codex","window":"primary","remaining":%v,"resets_at":%d}`,
		at.UnixMilli(), remaining, now.Add(5*time.Hour).UnixMilli())
}

func postBody(samples ...string) string {
	return `{"samples":[` + strings.Join(samples, ",") + `]}`
}

func decode[T any](t *testing.T, response httpResponse) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(response.Body, &out); err != nil {
		t.Fatalf("response body %q: %v", response.Body, err)
	}
	return out
}

type samplesPage struct {
	Samples []struct {
		At        int64   `json:"at"`
		Remaining float64 `json:"remaining"`
		ResetsAt  *int64  `json:"resets_at"`
	} `json:"samples"`
}

func TestRegisterDeclaresManagementPluginWithConfigFields(t *testing.T) {
	p := plugin.New(time.Now)
	path := filepath.Join(t.TempDir(), "samples.jsonl")

	reply := register(t, p, "plugin.register", "path: "+path+"\nretention-days: 10\n")

	var registration struct {
		SchemaVersion int `json:"schema_version"`
		Metadata      struct {
			Name         string
			ConfigFields []struct {
				Name    string
				Type    string
				Default any
			}
		} `json:"metadata"`
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err := json.Unmarshal(reply.Result, &registration); err != nil {
		t.Fatal(err)
	}
	if registration.SchemaVersion != 6 || registration.Metadata.Name != "quota-ledger" || !registration.Capabilities["management_api"] || !registration.Capabilities["usage_plugin"] {
		t.Fatalf("registration = %+v", registration)
	}
	fields := registration.Metadata.ConfigFields
	if len(fields) != 2 || fields[0].Name != "path" || fields[0].Type != "string" ||
		fields[1].Name != "retention-days" || fields[1].Type != "integer" || fields[1].Default != float64(35) {
		t.Fatalf("config fields = %+v", fields)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("ledger file not created: %v", err)
	}
}

func TestRegisterResolvesDefaultPathAgainstWorkingDirectory(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	register(t, plugin.New(time.Now), "plugin.register", "enabled: true\n")

	if _, err := os.Stat(filepath.Join(workDir, "quota-ledger", "samples.jsonl")); err != nil {
		t.Fatalf("default ledger file not created: %v", err)
	}
}

func TestRegisterRejectsInvalidConfig(t *testing.T) {
	for _, configYAML := range []string{"retention-days: 0\n", "retention-days: soon\n", "path: \"  \"\n"} {
		reply := register(t, plugin.New(time.Now), "plugin.register", configYAML)
		if reply.OK || reply.Error == nil || reply.Error.Code != "invalid_config" {
			t.Fatalf("register(%q) = %+v, want invalid_config", configYAML, reply)
		}
	}
}

func TestReconfigureMovesToNewPath(t *testing.T) {
	p, _ := registeredPlugin(t)
	manage(t, p, "POST", nil, postBody(sampleJSON(now, 50)))
	otherPath := filepath.Join(t.TempDir(), "other.jsonl")

	register(t, p, "plugin.reconfigure", "path: "+otherPath+"\n")

	page := decode[samplesPage](t, manage(t, p, "GET", nil, ""))
	if len(page.Samples) != 0 {
		t.Fatalf("samples after moving ledger = %+v, want none", page.Samples)
	}
}

func TestManagementRegisterDeclaresSampleRoutes(t *testing.T) {
	p, _ := registeredPlugin(t)

	reply := call(t, p, "management.register", map[string]any{"BasePath": "/v0/management"})

	var registration struct {
		Routes []struct {
			Method string
			Path   string
			Menu   string
		} `json:"routes"`
		Resources []any `json:"resources"`
	}
	if err := json.Unmarshal(reply.Result, &registration); err != nil {
		t.Fatal(err)
	}
	routes := registration.Routes
	if len(routes) != 2 || len(registration.Resources) != 0 ||
		routes[0].Method != "GET" || routes[0].Path != "/quota-ledger/samples" || routes[0].Menu != "" ||
		routes[1].Method != "POST" || routes[1].Path != "/quota-ledger/samples" {
		t.Fatalf("registration = %+v", registration)
	}
}

func TestPostedSamplesComeBackInOrder(t *testing.T) {
	p, _ := registeredPlugin(t)

	posted := manage(t, p, "POST", nil, postBody(
		sampleJSON(now, 40),
		sampleJSON(now.Add(-time.Hour), 60),
		sampleJSON(now.Add(-time.Hour+time.Minute), 60),
	))
	fetched := manage(t, p, "GET", url.Values{"since": {fmt.Sprint(now.Add(-2 * time.Hour).UnixMilli())}}, "")

	if posted.StatusCode != 200 || string(posted.Body) != `{"accepted":2,"skipped":1}` {
		t.Fatalf("POST = %d %s", posted.StatusCode, posted.Body)
	}
	page := decode[samplesPage](t, fetched)
	if fetched.StatusCode != 200 || len(page.Samples) != 2 ||
		page.Samples[0].At != now.Add(-time.Hour).UnixMilli() || page.Samples[1].Remaining != 40 ||
		*page.Samples[1].ResetsAt != now.Add(5*time.Hour).UnixMilli() {
		t.Fatalf("GET = %d %s", fetched.StatusCode, fetched.Body)
	}
}

func TestGetFiltersBySince(t *testing.T) {
	p, _ := registeredPlugin(t)
	manage(t, p, "POST", nil, postBody(sampleJSON(now.Add(-8*24*time.Hour), 90), sampleJSON(now.Add(-24*time.Hour), 70)))

	cases := map[string]struct {
		query url.Values
		want  int
	}{
		"defaults to the last 7 days": {query: nil, want: 1},
		"explicit since":              {query: url.Values{"since": {"0"}}, want: 2},
		"since after every sample":    {query: url.Values{"since": {fmt.Sprint(now.UnixMilli())}}, want: 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			page := decode[samplesPage](t, manage(t, p, "GET", tc.query, ""))
			if len(page.Samples) != tc.want {
				t.Fatalf("got %d samples, want %d", len(page.Samples), tc.want)
			}
		})
	}
}

func TestInvalidRequestsGetBadRequest(t *testing.T) {
	tooMany := make([]string, 2001)
	for i := range tooMany {
		tooMany[i] = sampleJSON(now.Add(-time.Duration(i)*time.Hour), 50)
	}
	cases := map[string]struct {
		method, body, wantError string
		query                   url.Values
	}{
		"body is not JSON":      {method: "POST", body: "nope", wantError: "invalid JSON body"},
		"samples missing":       {method: "POST", body: `{}`, wantError: "samples is required"},
		"too many samples":      {method: "POST", body: postBody(tooMany...), wantError: "at most 2000 samples"},
		"remaining is a string": {method: "POST", body: postBody(strings.Replace(sampleJSON(now, 1), `"remaining":1`, `"remaining":"1"`, 1)), wantError: "invalid JSON body"},
		"at is fractional":      {method: "POST", body: `{"samples":[{"at":1.5,"auth":"a","provider":"p","window":"w","remaining":1}]}`, wantError: "invalid JSON body"},
		"sample fails rules":    {method: "POST", body: postBody(sampleJSON(now, 50), sampleJSON(now, 101)), wantError: "samples[1]: remaining must be"},
		"sample from future":    {method: "POST", body: postBody(sampleJSON(now.Add(11*time.Minute), 50)), wantError: "samples[0]: at is more than 10 minutes"},
		"since is not a number": {method: "GET", query: url.Values{"since": {"yesterday"}}, wantError: "since must be"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p, _ := registeredPlugin(t)

			response := manage(t, p, tc.method, tc.query, tc.body)

			body := decode[struct{ Error string }](t, response)
			if response.StatusCode != 400 || !strings.Contains(body.Error, tc.wantError) {
				t.Fatalf("got %d %s, want 400 with %q", response.StatusCode, response.Body, tc.wantError)
			}
			if page := decode[samplesPage](t, manage(t, p, "GET", url.Values{"since": {"0"}}, "")); len(page.Samples) != 0 {
				t.Fatalf("rejected batch stored %d samples", len(page.Samples))
			}
		})
	}
}

func TestUnknownRouteIsNotFound(t *testing.T) {
	p, _ := registeredPlugin(t)

	response := manage(t, p, "DELETE", nil, "")

	if response.StatusCode != 404 {
		t.Fatalf("DELETE = %d %s, want 404", response.StatusCode, response.Body)
	}
}

func TestUnknownMethodReturnsErrorEnvelope(t *testing.T) {
	p, _ := registeredPlugin(t)

	reply := call(t, p, "executor.execute", map[string]any{})

	if reply.OK || reply.Error.Code != "unknown_method" {
		t.Fatalf("reply = %+v", reply)
	}
}
