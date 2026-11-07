package browser

import (
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/ydtg1993/papa/v3/pkg/egress"
)

// Security flags are applied after user flags: no proxy/DNS bypass is allowed.
func secureLauncher(l *launcher.Launcher, gateway string) {
	l.Proxy(gateway)
	for _, key := range []string{"disable-web-security", "allow-file-access-from-files", "allow-universal-access-from-files", "disable-site-isolation-trials", "disable-ipc-flooding-protection", "enable-features"} {
		l.Delete(flags.Flag(key))
	}
	l.Set(flags.Flag("proxy-bypass-list"), "<-loopback>")
	l.Set(flags.Flag("host-resolver-rules"), "MAP * ~NOTFOUND, EXCLUDE 127.0.0.1")
	for _, key := range []string{"disable-quic", "disable-background-networking", "disable-extensions", "disable-component-update", "disable-sync", "no-first-run", "no-default-browser-check"} {
		l.Set(flags.Flag(key))
	}
	l.Delete(flags.Flag("no-proxy-server"))
	l.Delete(flags.Flag("proxy-auto-detect"))
	l.Delete(flags.Flag("proxy-pac-url"))
	l.Set(flags.Flag("force-webrtc-ip-handling-policy"), "disable_non_proxied_udp")
	l.Set(flags.Flag("webrtc-ip-handling-policy"), "disable_non_proxied_udp")
	l.Set(flags.Flag("disable-features"), "MediaRouter,OptimizationHints,DnsOverHttps,WebTransport,DirectSockets,SpeculationRulesPrefetch,Prerender2")
}
func validateNavigation(raw string) error { _, _, err := egress.Normalize(raw); return err }
