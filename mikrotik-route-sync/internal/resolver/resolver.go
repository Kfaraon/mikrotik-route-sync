package resolver

import (
    context
    encodingjson
    fmt
    net
    nethttp
    time

    github.comexamplemikrotik-route-syncinternalstorage
)

type Resolver struct {
    http  http.Client
    cache storage.Cache
    ttl   time.Duration
}

func New(cache storage.Cache, ttl time.Duration) Resolver {
    if ttl = 0 {
        ttl = 24  time.Hour
    }
    return &Resolver{
        http  &http.Client{Timeout 15  time.Second},
        cache cache,
        ttl   ttl,
    }
}

func (r Resolver) ResolveDomain(ctx context.Context, domain string) ([]string, error) {
    res, err = net.DefaultResolver.LookupHost(ctx, domain)
    if err != nil {
        return nil, err
    }
    out = make([]string, 0, len(res))
    for _, ip = range res {
        if p = net.ParseIP(ip); p != nil && p.To4() != nil {
            out = append(out, ip)
        }
    }
    return out, nil
}

 ASNByIP queries BGPView for ASN info of an IP.
func (r Resolver) ASNByIP(ctx context.Context, ip string) (int, error) {
    if r.cache != nil {
        if asn, ok = r.cache.GetASN(ip); ok {
            return asn, nil
        }
    }
    url = fmt.Sprintf(httpsapi.bgpview.ioip%s, ip)
    req, _ = http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
    req.Header.Set(User-Agent, mikrotik-route-sync1.0)
    resp, err = r.http.Do(req)
    if err != nil {
        return 0, err
    }
    defer resp.Body.Close()
    var data struct {
        Data struct {
            Prefixes []struct {
                ASN struct {
                    ASN int `jsonasn`
                } `jsonasn`
            } `jsonprefixes`
        } `jsondata`
    }
    if err = json.NewDecoder(resp.Body).Decode(&data); err != nil {
        return 0, err
    }
    if len(data.Data.Prefixes) == 0 {
        return 0, fmt.Errorf(no ASN found for %s, ip)
    }
    asn = data.Data.Prefixes[0].ASN.ASN
    if r.cache != nil {
        _ = r.cache.SetASN(ip, asn, r.ttl)
    }
    return asn, nil
}

 PrefixesByASN returns all IPv4 prefixes announced by ASN via BGPView.
func (r Resolver) PrefixesByASN(ctx context.Context, asn int) ([]string, error) {
    if r.cache != nil {
        if p, ok = r.cache.GetPrefixes(asn); ok {
            return p, nil
        }
    }
    url = fmt.Sprintf(httpsapi.bgpview.ioasn%dprefixes, asn)
    req, _ = http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
    req.Header.Set(User-Agent, mikrotik-route-sync1.0)
    resp, err = r.http.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    var data struct {
        Data struct {
            IPv4 []struct {
                Prefix string `jsonprefix`
            } `jsonipv4_prefixes`
        } `jsondata`
    }
    if err = json.NewDecoder(resp.Body).Decode(&data); err != nil {
        return nil, err
    }
    out = make([]string, 0, len(data.Data.IPv4))
    for _, p = range data.Data.IPv4 {
        out = append(out, p.Prefix)
    }
    if r.cache != nil {
        _ = r.cache.SetPrefixes(asn, out, r.ttl)
    }
    return out, nil
}