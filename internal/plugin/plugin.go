package plugin

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/Silthus/cliproxy-quota-ledger/internal/ledger"
)

const (
	id                                 = "quota-ledger"
	rawManagementResponseSchemaVersion = 6
)

var Version = "0.1.0"

type Plugin struct {
	now    func() time.Time
	mu     sync.Mutex
	config config
	store  *ledger.Store
}

func New(now func() time.Time) *Plugin {
	return &Plugin{now: now}
}

type envelope struct {
	OK     bool           `json:"ok"`
	Result any            `json:"result,omitempty"`
	Error  *envelopeError `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (p *Plugin) Call(method string, request []byte) (reply []byte, ok bool) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return p.configure(request)
	case "plugin.quiesce":
		return success(struct{}{})
	case "management.register":
		return success(managementRoutes())
	case "management.handle":
		return p.handleManagement(request)
	case "usage.handle":
		return p.handleUsage(request)
	default:
		return failure("unknown_method", "unknown method: "+method)
	}
}

func (p *Plugin) configure(request []byte) ([]byte, bool) {
	var lifecycle struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if err := json.Unmarshal(request, &lifecycle); err != nil {
		return failure("invalid_request", err.Error())
	}
	next, err := parseConfig(lifecycle.ConfigYAML)
	if err != nil {
		return failure("invalid_config", err.Error())
	}
	if err := p.useConfig(next); err != nil {
		return failure("store_unavailable", err.Error())
	}
	return success(registration())
}

func (p *Plugin) useConfig(next config) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.store != nil && p.config == next {
		return nil
	}
	store, err := ledger.Open(next.path, next.retention, p.now)
	if err != nil {
		return err
	}
	p.config, p.store = next, store
	return nil
}

func (p *Plugin) currentStore() *ledger.Store {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.store
}

func registration() any {
	return map[string]any{
		"schema_version": rawManagementResponseSchemaVersion,
		"metadata": map[string]any{
			"Name":             id,
			"Version":          Version,
			"Author":           "Silthus",
			"GitHubRepository": "https://github.com/danielphan2003/skid-quota-ledger",
			"Logo":             "https://github.com/Silthus.png",
			"ConfigFields":     configFields,
		},
		"capabilities": map[string]bool{"management_api": true, "usage_plugin": true},
	}
}

func success(result any) ([]byte, bool) {
	raw, err := json.Marshal(envelope{OK: true, Result: result})
	if err != nil {
		return failure("encode_failed", err.Error())
	}
	return raw, true
}

func failure(code, message string) ([]byte, bool) {
	raw, _ := json.Marshal(envelope{Error: &envelopeError{Code: code, Message: message}})
	return raw, false
}
