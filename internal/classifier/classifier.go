package classifier

import "strings"

type Result struct {
	Methods    []string
	StaticURLs []string
	Domains    []string
	ASN        string
}

func Classify(service string, overrideMethod string) Result {
	if overrideMethod != "" {
		return Result{Methods: []string{overrideMethod}}
	}
	s := strings.ToLower(service)
	switch s {
	case "cloudflare":
		return Result{
			Methods:    []string{"cdn"},
			StaticURLs: []string{"https://www.cloudflare.com/ips-v4", "https://www.cloudflare.com/ips-v6"},
			ASN:        "AS13335",
		}
	case "youtube", "google":
		return Result{
			Methods: []string{"dynamic", "cdn"},
			Domains: []string{"youtube.com", "googlevideo.com", "ytimg.com"},
			ASN:     "AS15169",
		}
	case "telegram":
		return Result{
			Methods: []string{"dynamic", "asn"},
			Domains: []string{"telegram.org"},
		}
	case "fastly":
		return Result{
			Methods:    []string{"cdn"},
			StaticURLs: []string{"https://api.fastly.com/public-ip-list"},
		}
	case "akamai":
		return Result{
			Methods: []string{"akamai"},
			ASN:     "AS20940",
		}
	}
	
	// Проверка ASN напрямую
	if strings.HasPrefix(strings.ToUpper(service), "AS") {
		return Result{
			Methods: []string{"asn"},
			ASN:     strings.ToUpper(service),
		}
	}
	
	// Проверка AS20940 (Akamai)
	if service == "AS20940" || service == "20940" {
		return Result{
			Methods: []string{"akamai"},
			ASN:     "AS20940",
		}
	}
	
	// Домен → whois
	if strings.Contains(service, ".") {
		return Result{
			Methods: []string{"whois"},
			Domains: []string{service},
		}
	}
	
	// По умолчанию → dynamic
	return Result{
		Methods: []string{"dynamic"},
		Domains: []string{service},
	}
}
