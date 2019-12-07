// Package forward implements a forwarding proxy. It caches an upstream net.Conn for some time, so if the same
// client returns the upstream's Conn will be precached. Depending on how you benchmark this looks to be
// 50% faster than just opening a new connection for every client. It works with UDP and TCP and uses
// inband healthchecking.
package forward

import (
	"context"
	"crypto/tls"
	"errors"
	"sync"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/debug"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/request"

	"github.com/miekg/dns"
	ot "github.com/opentracing/opentracing-go"
)

var log = clog.NewWithPlugin("forward")

// Forward represents a plugin instance that can proxy requests to another (DNS) server. It has a list
// of proxies each representing one upstream proxy.
type Forward struct {
	proxies    []*Proxy
	p          Policy
	hcInterval time.Duration

	from    string
	ignored []string

	tlsConfig     *tls.Config
	tlsServerName string
	maxfails      uint32
	expire        time.Duration

	opts options // also here for testing

	Next plugin.Handler
}

// New returns a new Forward.
func New() *Forward {
	f := &Forward{maxfails: 2, tlsConfig: new(tls.Config), expire: defaultExpire, p: new(random), from: ".", hcInterval: hcInterval}
	return f
}

// SetProxy appends p to the proxy list and starts healthchecking.
func (f *Forward) SetProxy(p *Proxy) {
	f.proxies = append(f.proxies, p)
	p.start(f.hcInterval)
}

// Len returns the number of configured proxies.
func (f *Forward) Len() int { return len(f.proxies) }

// Name implements plugin.Handler.
func (f *Forward) Name() string { return "forward" }

type fwdResp struct {
	ret         *dns.Msg
	code        int
	upstreamErr error
}

// ServeDNS implements plugin.Handler.
func (f *Forward) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {

	state := request.Request{W: w, Req: r}
	if !f.match(state) {
		return plugin.NextOrFailure(f.Name(), f.Next, ctx, w, r)
	}

	var span ot.Span
	span = ot.SpanFromContext(ctx)
	list := f.List()

	live := make([]*Proxy, 0, len(list))
	for _, proxy := range list {
		if proxy.Down(f.maxfails) {
			continue
		}
		live = append(live, proxy)
	}

	wg := &sync.WaitGroup{}
	ch := make(chan fwdResp, len(live))

	for _, proxy := range live {
		wg.Add(1)
		go func(proxy *Proxy) {
			defer wg.Done()
			var child ot.Span
			var ctxInner context.Context
			var fails uint32 = 0

			for fails < f.maxfails {
				if span != nil {
					child = span.Tracer().StartSpan("connect", ot.ChildOf(span.Context()))
					ctxInner = ot.ContextWithSpan(ctx, child)
				}

				var (
					ret *dns.Msg
					err error
				)

				opts := f.opts
				for {
					ret, err = proxy.Connect(ctxInner, state, opts)
					if err == ErrCachedClosed { // Remote side closed conn, can only happen with TCP.
						continue
					}
					// Retry with TCP if truncated and prefer_udp configured.
					if ret != nil && ret.Truncated && !opts.forceTCP && opts.preferUDP {
						opts.forceTCP = true
						continue
					}
					break
				}

				if child != nil {
					child.Finish()
				}

				if err != nil {
					// Kick off health check to see if *our* upstream is broken.
					if f.maxfails != 0 {
						proxy.Healthcheck()
					}

					fails++
					if !proxy.Down(f.maxfails) {
						continue
					}

					ch <- fwdResp{
						ret:         nil,
						code:        0,
						upstreamErr: err,
					}
					break
				}

				if !state.Match(ret) {
					debug.Hexdumpf(ret, "Wrong reply for id: %d, %s %d", ret.Id, state.QName(), state.QType())

					formerr := new(dns.Msg)
					formerr.SetRcode(state.Req, dns.RcodeFormatError)
					ch <- fwdResp{
						ret:         formerr,
						code:        0,
						upstreamErr: nil,
					}
					break
				} else {
					ch <- fwdResp{
						ret:         ret,
						code:        0,
						upstreamErr: nil,
					}
					break
				}
			}
		}(proxy)
	}

	wg.Wait()
	close(ch)

	resps := make([]fwdResp, 0, len(live))
	for resp := range ch {
		resps = append(resps, resp)
	}

	var successRet *dns.Msg
	ipAnswers := make([]dns.RR, 0, len(live))
	for _, resp := range resps {
		if resp.ret == nil {
			continue
		}
		for _, rr := range resp.ret.Answer {
			switch rr.Header().Rrtype {
			case dns.TypeA:
				ipAnswers = append(ipAnswers, rr)
				successRet = resp.ret
			case dns.TypeAAAA:
				ipAnswers = append(ipAnswers, rr)
				successRet = resp.ret
			}
		}
	}

	if len(ipAnswers) > 0 {
		successRet.Answer = ipAnswers
		w.WriteMsg(successRet)
		return 0, nil
	}

	for _, resp := range resps {
		if resp.upstreamErr == nil {
			continue
		}

		return dns.RcodeServerFailure, resp.upstreamErr
	}

	return dns.RcodeServerFailure, ErrNoHealthy
}

func (f *Forward) match(state request.Request) bool {
	if !plugin.Name(f.from).Matches(state.Name()) || !f.isAllowedDomain(state.Name()) {
		return false
	}

	return true
}

func (f *Forward) isAllowedDomain(name string) bool {
	if dns.Name(name) == dns.Name(f.from) {
		return true
	}

	for _, ignore := range f.ignored {
		if plugin.Name(ignore).Matches(name) {
			return false
		}
	}
	return true
}

// ForceTCP returns if TCP is forced to be used even when the request comes in over UDP.
func (f *Forward) ForceTCP() bool { return f.opts.forceTCP }

// PreferUDP returns if UDP is preferred to be used even when the request comes in over TCP.
func (f *Forward) PreferUDP() bool { return f.opts.preferUDP }

// List returns a set of proxies to be used for this client depending on the policy in f.
func (f *Forward) List() []*Proxy { return f.p.List(f.proxies) }

var (
	// ErrNoHealthy means no healthy proxies left.
	ErrNoHealthy = errors.New("no healthy proxies")
	// ErrNoForward means no forwarder defined.
	ErrNoForward = errors.New("no forwarder defined")
	// ErrCachedClosed means cached connection was closed by peer.
	ErrCachedClosed = errors.New("cached connection was closed by peer")
)

// options holds various options that can be set.
type options struct {
	forceTCP  bool
	preferUDP bool
}

const defaultTimeout = 5 * time.Second
