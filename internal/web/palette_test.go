package web

// Тест контраста палитры: цвета интерфейса обязаны проходить WCAG AA (4.5:1)
// для текста. Палитра взята из opencode, но светлая тема затемнена —
// исходные светлые значения (#3b7dd8, #d68c27, #3d9a57, #318795) на #f8f8f8
// дают 3.87/2.59/3.32/3.93 и не проходят AA для мелкого текста.
//
// Тест читает реальный app.css, поэтому поймает и случайную правку цвета,
// и новую пару «текст на фоне», добавленную без проверки.

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func cssBlock(t *testing.T, css, sel string) string {
	t.Helper()
	i := strings.Index(css, sel)
	if i < 0 {
		t.Fatalf("block %q not found", sel)
	}
	j := strings.Index(css[i:], "}")
	if j < 0 {
		t.Fatalf("block %q not terminated", sel)
	}
	return css[i : i+j]
}

func cssVars(block string) map[string]string {
	out := map[string]string{}
	for _, m := range regexp.MustCompile(`--([a-z0-9-]+):\s*(#[0-9a-fA-F]{6})`).FindAllStringSubmatch(block, -1) {
		out[m[1]] = m[2]
	}
	return out
}

func relLum(hex string) float64 {
	hex = strings.TrimPrefix(hex, "#")
	v, _ := strconv.ParseUint(hex, 16, 32)
	ch := [3]float64{
		float64(v>>16&0xff) / 255,
		float64(v>>8&0xff) / 255,
		float64(v&0xff) / 255,
	}
	var lin [3]float64
	for i, c := range ch {
		if c <= 0.03928 {
			lin[i] = c / 12.92
		} else {
			lin[i] = math.Pow((c+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*lin[0] + 0.7152*lin[1] + 0.0722*lin[2]
}

func contrast(a, b string) float64 {
	l1, l2 := relLum(a), relLum(b)
	if l1 < l2 {
		l1, l2 = l2, l1
	}
	return (l1 + 0.05) / (l2 + 0.05)
}

// TestPaletteContrast — все пары «текст/фон» из темы проходят AA.
func TestPaletteContrast(t *testing.T) {
	raw, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	css := string(raw)

	themes := []struct {
		name string
		vars map[string]string
	}{
		{"light", cssVars(cssBlock(t, css, ":root {"))},
		{"dark", cssVars(cssBlock(t, css, ".dark {"))},
	}

	pairs := [][3]string{
		{"текст на фоне", "text", "bg"},
		{"текст на панели", "text", "bg-alt"},
		{"приглушённый", "muted", "bg"},
		{"вторичный текст", "text-light", "bg-alt"},
		{"primary на фоне", "primary", "bg"},
		{"бейдж OK", "badge-ok-text", "badge-ok-bg"},
		{"бейдж ошибки", "badge-err-text", "badge-err-bg"},
		{"бейдж предупр.", "badge-warn-text", "badge-warn-bg"},
		{"бейдж авто", "badge-auto-text", "badge-auto-bg"},
		{"бейдж override", "badge-override-text", "badge-override-bg"},
		{"бейдж muted", "badge-muted-text", "badge-muted-bg"},
		{"diff добавлено", "diff-add-text", "diff-add-bg"},
		{"diff удалено", "diff-del-text", "diff-del-bg"},
		{"diff без изм.", "diff-same-text", "diff-same-bg"},
		{"лог: текст", "log-text", "log-bg"},
		{"лог: время", "log-time", "log-bg"},
		// Кнопки: текст поверх акцентной заливки. Заливки (--fill-*) общие
		// для обеих тем, текст на них тёмный — светлая тема повторяет вид
		// тёмной, а не пишет белым по тёмно-синему.
		{"кнопка primary", "on-primary", "fill-primary"},
		{"кнопка primary hover", "on-primary", "fill-primary-hover"},
		{"кнопка danger", "on-danger", "fill-danger"},
		{"тост success", "on-success", "fill-success"},
		{"тост error", "on-danger", "fill-danger"},
		{"outline (текст)", "primary", "bg"},
		// Уровни логов: заливки общие для тем, текст на них тёмный.
		{"лог INFO", "on-info", "fill-info"},
		{"лог WARN", "on-warning", "fill-warning"},
		{"лог ERROR", "on-danger", "fill-danger"},
		{"лог DEBUG", "on-muted", "fill-muted"},
		{"secondary (текст)", "text", "control-bg"},
	}

	// Границы контролов — нетекстовые элементы: WCAG 1.4.11 требует 3:1,
	// а не 4.5:1. Раньше --border (#d3d3d3) давал 1.41 на фоне страницы,
	// и кнопки/поля сливались с карточками.
	borderPairs := [][3]string{
		{"secondary (граница)", "control-border", "bg-alt"},
		{"поле ввода (граница)", "control-border", "bg"},
		{"кнопка secondary на панели", "control-border", "control-bg-hover"},
		// Заливка мягкая (на белом 1.7-2.4:1), поэтому у кнопок есть
		// контур с контрастом ≥3:1 к странице.
		{"кнопка primary (граница)", "fill-primary-border", "bg"},
		{"кнопка danger (граница)", "fill-danger-border", "bg"},
		{"кнопка warning (граница)", "fill-warning-border", "bg"},
		{"кнопка success (граница)", "fill-success-border", "bg"},
	}

	for _, th := range themes {
		for _, p := range pairs {
			fg, okF := th.vars[p[1]]
			bg, okB := th.vars[p[2]]
			if !okF || !okB {
				t.Errorf("%s: %s — переменные %s/%s не найдены", th.name, p[0], p[1], p[2])
				continue
			}
			if got := contrast(fg, bg); got < 4.5 {
				t.Errorf("%s: %s (%s на %s) = %.2f < 4.5 (WCAG AA)", th.name, p[0], fg, bg, got)
			}
		}
		for _, p := range borderPairs {
			fg, okF := th.vars[p[1]]
			bg, okB := th.vars[p[2]]
			if !okF || !okB {
				t.Errorf("%s: %s — переменные %s/%s не найдены", th.name, p[0], p[1], p[2])
				continue
			}
			if got := contrast(fg, bg); got < 3.0 {
				t.Errorf("%s: %s (%s на %s) = %.2f < 3.0 (WCAG 1.4.11 non-text)", th.name, p[0], fg, bg, got)
			}
		}
	}
}

// TestPaletteSelfCheck — сама реализация контраста не должна врать:
// известные значения из WCAG (чёрный на белом = 21, белый на белом = 1).
func TestPaletteSelfCheck(t *testing.T) {
	cases := []struct {
		fg, bg string
		want   float64
	}{
		{"#000000", "#ffffff", 21},
		{"#ffffff", "#ffffff", 1},
		{"#777777", "#ffffff", 4.48}, // граница AA для крупного текста
	}
	for _, c := range cases {
		got := contrast(c.fg, c.bg)
		if fmt.Sprintf("%.2f", got) != fmt.Sprintf("%.2f", c.want) {
			t.Errorf("contrast(%s,%s) = %.2f, want %.2f", c.fg, c.bg, got, c.want)
		}
	}
}

// TestLogLevelsUseThemePalette — значки уровней логов обязаны брать цвета из
// переменных темы. Регресс: они были захардкожены (#1e40af/#b45309/#dc2626/
// #4b5563) и в тёмной теме выбивались из палитры opencode.
func TestLogLevelsUseThemePalette(t *testing.T) {
	raw, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	css := string(raw)

	want := map[string][2]string{
		".level-debug": {"--fill-muted", "--on-muted"},
		".level-info":  {"--fill-info", "--on-info"},
		".level-warn":  {"--fill-warning", "--on-warning"},
		".level-error": {"--fill-danger", "--on-danger"},
	}
	for sel, vars := range want {
		block := cssBlock(t, css, sel+" {")
		for _, v := range vars {
			if !strings.Contains(block, "var("+v+")") {
				t.Errorf("%s must use var(%s): %s", sel, v, block)
			}
		}
	}

	// Старые жёсткие цвета не должны вернуться.
	for _, hard := range []string{"#1e40af", "#b45309", "#dc2626", "#4b5563"} {
		if strings.Contains(css, hard) {
			t.Errorf("log level styles must not hardcode %s", hard)
		}
	}
}

// TestControlBordersVisible — границы кнопок и полей ввода берут
// --control-border, а не --border: #d3d3d3 на белом даёт 1.5:1, и контрол
// сливался с карточкой (WCAG 1.4.11 требует 3:1 для нетекстовых элементов).
func TestControlBordersVisible(t *testing.T) {
	raw, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	css := string(raw)

	for _, sel := range []string{
		"input[type=\"text\"],\ninput[type=\"search\"],\ninput[type=\"password\"],\ninput[type=\"number\"],\nselect,\ntextarea {",
		"button.btn.btn-secondary {",
	} {
		block := cssBlock(t, css, sel)
		if !strings.Contains(block, "var(--control-border)") {
			t.Errorf("%q must use --control-border: %s", sel, block)
		}
	}
}

// TestFilledButtonsMatchBothThemes — залитые кнопки выглядят одинаково в
// светлой и тёмной теме: мягкая заливка + тёмный текст. Регресс: в светлой
// теме заливка была тёмно-синей с белой подписью, и интерфейс в двух темах
// выглядел «инвертированным».
func TestFilledButtonsMatchBothThemes(t *testing.T) {
	raw, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	css := string(raw)

	light := cssVars(cssBlock(t, css, ":root {"))
	dark := cssVars(cssBlock(t, css, ".dark {"))

	for _, v := range []string{"fill-primary", "fill-primary-hover", "fill-danger", "fill-warning", "fill-success", "fill-info", "fill-muted"} {
		if light[v] == "" || dark[v] == "" {
			t.Errorf("variable --%s must be defined in both themes", v)
			continue
		}
		if light[v] != dark[v] {
			t.Errorf("--%s differs: light %s, dark %s (fills must be shared)", v, light[v], dark[v])
		}
	}
	for _, v := range []string{"on-primary", "on-danger", "on-success", "on-warning"} {
		if light[v] != "#141414" || dark[v] != "#141414" {
			t.Errorf("--%s must be #141414 in both themes, got light %q dark %q", v, light[v], dark[v])
		}
	}

	for _, sel := range []string{"button,\ninput[type=\"submit\"] {", "button.btn {", "button.btn.btn-primary {"} {
		block := cssBlock(t, css, sel)
		if !strings.Contains(block, "var(--fill-primary)") {
			t.Errorf("%q must use --fill-primary: %s", sel, block)
		}
		if !strings.Contains(block, "var(--on-primary)") {
			t.Errorf("%q must use --on-primary: %s", sel, block)
		}
	}
}
