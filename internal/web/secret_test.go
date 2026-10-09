package web

import "testing"

// H7: ключи реестра хранятся с `_` ("telegram.bot_token"), и IsSecret
// должен их находить. Прежняя версия заменяла `_` на `.` только у запроса,
// поэтому телеграм-токен/пароль mikrotik не распознавались как секреты.
func TestIsSecretMatchesRegistryKeys(t *testing.T) {
	secret := []string{
		"mikrotik.password",
		"telegram.bot_token",
		"web.auth.password",
		"external.akamai_api_key",
		"external.urlscan_api_key",
		"mikrotik.password_file",
		"telegram.bot_token_file",
		"web.auth.password_file",
		"external.akamai_api_key_file",
	}
	for _, k := range secret {
		if !IsSecret(k) {
			t.Errorf("IsSecret(%q)=false, want true", k)
		}
	}
	// Регистронезависимость и нормализация разделителей.
	if !IsSecret("Telegram.Bot_Token") {
		t.Error("case/separator normalization broken")
	}
	if !IsSecret("  telegram.bot_token  ") {
		t.Error("whitespace trim broken")
	}
	// Не-секреты.
	for _, k := range []string{"mikrotik.host", "firewall.address_list", "", "telegram.enabled"} {
		if IsSecret(k) {
			t.Errorf("IsSecret(%q)=true, want false", k)
		}
	}
}

func TestRedactMapMasksRegistrySecrets(t *testing.T) {
	in := map[string]string{
		"telegram.bot_token": "123456:ABC",
		"mikrotik.password":  "hunter2",
		"mikrotik.host":      "10.0.0.1",
	}
	out := RedactMap(in)
	if out["mikrotik.host"] != "10.0.0.1" {
		t.Errorf("non-secret mangled: %q", out["mikrotik.host"])
	}
	if out["telegram.bot_token"] == in["telegram.bot_token"] {
		t.Error("bot token not masked")
	}
	if out["mikrotik.password"] == in["mikrotik.password"] {
		t.Error("password not masked")
	}
}
