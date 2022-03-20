package checker

import (
	"bufio"
	"context"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/nfx/slrp/app"
	"github.com/nfx/slrp/pmux"

	"github.com/corpix/uarand"
	"github.com/microcosm-cc/bluemonday"
)

type Checker interface {
	Check(ctx context.Context, proxy pmux.Proxy) (time.Duration, error)
}

var (
	firstPass = []string{
		// these check for ext ip, but don't show headers
		"https://ifconfig.me/ip",
		"https://ifconfig.io/ip",
		"https://myexternalip.com/raw",
		"https://ipv4.icanhazip.com/",
		"https://ipinfo.io/ip",
		"https://api.ipify.org/",
		"https://wtfismyip.com/text",
	}
	secondPass = map[string]string{
		// checks for X-Forwarded-For and alikes
		"https://ifconfig.me/all":      "user_agent",
		"https://ifconfig.io/all.json": "ifconfig_hostname",
	}
	ipRegex            = regexp.MustCompile(`(?m)^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$`)
	errCloudFlare      = temporary("cloudflare captcha")
	errGoogleRatelimit = temporary("google ratelimit")
	ErrNotAnonymous    = fmt.Errorf("this IP address found")
)

func NewChecker() Checker {
	ip, err := thisIP()
	if err != nil {
		panic(fmt.Errorf("cannot get this IP: %w", err))
	}
	discardingTransport := pmux.ContextualHttpTransport()
	discardingTransport.DisableKeepAlives = true
	discardingTransport.MaxIdleConns = 0
	client := &http.Client{
		Transport: discardingTransport,
		Timeout:   5 * time.Second,
	}
	return &configurableChecker{
		ip: ip,
		client: client,
		strategies: map[string]Checker{
			"twopass": newTwoPass(ip, client),
			"simple": newFederated(firstPass, client, ip),
		},
		strategy: "simple",
	}
}

type configurableChecker struct {
	ip string
	client *http.Client
	strategies map[string]Checker
	strategy string
}

func (cc *configurableChecker) Configure(conf app.Config) error {
	cc.strategy = conf.StrOr("strategy", "simple")
	cc.client.Timeout = conf.DurOr("timeout", 5 * time.Second)
	return nil
}

func (cc *configurableChecker) Check(ctx context.Context, proxy pmux.Proxy) (time.Duration, error) {
	return cc.strategies[cc.strategy].Check(ctx, proxy)
}

func newTwoPass(ip string, client *http.Client) twoPass {
	var res twoPass
	for _, v := range firstPass {
		res.first = append(res.first, &simple{
			client: client,
			page:   v,
			ip:     ip,
		})
	}
	for k, v := range secondPass {
		res.second = append(res.second, &simple{
			client: client,
			page:   k,
			valid:  v,
			ip:     ip,
		})
	}
	return res
}

type twoPass struct {
	first  federated
	second federated
}

func (f twoPass) Check(ctx context.Context, proxy pmux.Proxy) (time.Duration, error) {
	t, err := f.first.Check(ctx, proxy)
	if isTimeout(err) {
		return t, err
	}
	if err != nil {
		return t, fmt.Errorf("first: %w", err)
	}
	t, err = f.second.Check(ctx, proxy)
	if isTimeout(err) {
		return t, err
	}
	if err != nil {
		return t, fmt.Errorf("second: %w", err)
	}
	return t, nil
}

type federated []*simple

func newFederated(sites []string, client *http.Client, ip string) (out federated) {
	for _, v := range firstPass {
		out = append(out, &simple{
			client: client,
			page:   v,
			ip:     ip,
		})
	}
	return out
}

func (f federated) Check(ctx context.Context, proxy pmux.Proxy) (time.Duration, error) {
	choice := rand.Intn(len(f))
	return f[choice].Check(ctx, proxy)
}

type simple struct {
	client *http.Client
	page   string
	valid  string
	ip     string
}

func (sc *simple) Check(ctx context.Context, proxy pmux.Proxy) (time.Duration, error) {
	start := time.Now()
	req, err := http.NewRequestWithContext(proxy.InContext(ctx), "GET", sc.page, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", uarand.GetRandom())
	res, err := sc.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return 0, err
	}
	err = sc.validate(string(body))
	if isTimeout(err) {
		return 0, err
	}
	if err != nil {
		return 0, err
	}
	return time.Now().Sub(start), nil // TODO: speed is always the same?...
}

func (sc *simple) validate(body string) error {
	// Maximum number of open connections reached
	// Too Many Requests
	if strings.Contains(body, "client does not have permission to get URL") {
		return errGoogleRatelimit
	}
	if strings.Contains(body, "Cloudflare") {
		return errCloudFlare
	}
	if strings.Contains(body, sc.ip) {
		return ErrNotAnonymous
	}
	if sc.valid == "" && !ipRegex.MatchString(body) {
		return fmt.Errorf("invalid response received: %s", truncatedBody(body))
	}
	if !strings.Contains(body, sc.valid) {
		return fmt.Errorf("invalid response received: %s", truncatedBody(body))
	}
	return nil
}

var sanitize = bluemonday.StrictPolicy()

func truncatedBody(body string) string {
	body = sanitize.Sanitize(body)
	body = app.Shrink(body)
	cutoff := 512
	if len(body) > cutoff {
		return body[:cutoff] + fmt.Sprintf(" (%db more)", len(body)-cutoff)
	}
	return body
}

func thisIP() (string, error) {
	r, err := http.Get("https://ifconfig.me/ip")
	if err != nil {
		return "", err
	}
	defer r.Body.Close()
	s := bufio.NewScanner(r.Body)
	s.Scan()
	return s.Text(), nil
}

type temporary string

func (t temporary) Temporary() bool {
	return true
}

func (t temporary) Error() string {
	return string(t)
}

func isTimeout(err error) bool {
	// put timeouts into later retry queue
	t, ok := err.(interface {
		Temporary() bool
	})
	return ok && t.Temporary()
}
