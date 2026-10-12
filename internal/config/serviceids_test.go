package config

import (
	"strings"
	"testing"
)

// TestServiceIDsRoundTrip — карта service_ids сохраняется и читается обратно,
// а пустая карта не засоряет YAML.
func TestServiceIDsRoundTrip(t *testing.T) {
	path := writeTempConfig(t, minimalYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	const id = "11111111-1111-4111-8111-111111111111"
	if cfg.ServiceIDs == nil {
		cfg.ServiceIDs = map[string]string{}
	}
	cfg.ServiceIDs["youtube.com"] = id
	if err := cfg.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ServiceIDs["youtube.com"] != id {
		t.Fatalf("service_ids lost on reload: %+v", reloaded.ServiceIDs)
	}
}

// TestValidateRejectsBadServiceIDName — ключи service_ids проверяются как
// имена сервисов.
func TestValidateRejectsBadServiceIDName(t *testing.T) {
	body := strings.Replace(minimalYAML,
		"services: [cloudflare]",
		"services: [cloudflare]\nservice_ids: {\"Bad Name\": x}",
		1)
	if _, err := Load(writeTempConfig(t, body)); err == nil {
		t.Fatal("expected invalid service_ids name rejection")
	}
}
