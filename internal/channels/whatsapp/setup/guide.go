package setup

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/slimslenderslacks/work/internal/channels/whatsapp"
)

// Guide is the data the post-setup instructions are rendered from.
type Guide struct {
	Webhook       whatsapp.WebhookConfig
	PublicURL     string // base URL of the tunnel, if known ("" prints a placeholder)
	VerifyToken   string // the one secret that is shown: Meta asks for it
	PhoneNumberID string
	WABAID        string
	AllowFrom     []string
	ConfigPath    string
}

// LocalBaseURL is where the webhook listens on this machine; a wildcard bind
// address is shown as loopback.
func LocalBaseURL(host string, port int) string {
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(port))
}

// WebhookURL is the callback URL to register with Meta: PublicURL (or a
// placeholder) plus the webhook path.
func (g Guide) WebhookURL() string {
	base := strings.TrimRight(strings.TrimSpace(g.PublicURL), "/")
	if base == "" {
		base = "https://<your-tunnel-host>"
	}
	return base + g.Webhook.Path
}

// WriteInstructions prints what has to happen outside setup: expose the
// listener through a tunnel, register the callback URL and verify token in
// the Meta developer console, and add the owner to the recipient list.
//
// The verify token is printed in full because it must be pasted into Meta; it
// authenticates only the subscription handshake. The access token and app
// secret are never printed.
func WriteInstructions(w io.Writer, g Guide) {
	local := LocalBaseURL(g.Webhook.Host, g.Webhook.Port)
	p := func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }

	p("")
	p("Next steps")
	p("----------")
	p("1. Expose the webhook listener over HTTPS. Meta only calls public https URLs,")
	p("   and the listener binds %s, so front it with a tunnel, e.g.:", net.JoinHostPort(g.Webhook.Host, strconv.Itoa(g.Webhook.Port)))
	p("")
	p("     cloudflared tunnel --url %s", local)
	p("     (or: ngrok http %d, tailscale funnel %d)", g.Webhook.Port, g.Webhook.Port)
	p("")
	p("   Note the https://... address the tunnel prints.")
	p("")
	p("2. In the Meta developer console: App Dashboard > WhatsApp > Configuration >")
	p("   Webhook > Edit, and enter:")
	p("")
	p("     Callback URL:  %s", g.WebhookURL())
	p("     Verify token:  %s", g.VerifyToken)
	p("")
	p("   Click 'Verify and save', then 'Manage' and subscribe to the 'messages' field.")
	if g.PublicURL == "" {
		p("   (Re-run with --public-url https://<tunnel-host> to print the exact callback URL.)")
	}
	p("")
	p("3. Add the owner's phone to Meta's recipient list while the app is in development")
	p("   mode: App Dashboard > WhatsApp > API Setup > 'To' > Manage phone number list.")
	if len(g.AllowFrom) == 0 {
		p("   WARNING: no owner numbers are on the allowlist, so every inbound message will be")
		p("   denied. Re-run `orch whatsapp setup --allow-from <number>`.")
	}
	p("")
	p("4. Run the daemon with the WhatsApp channel enabled, then check it:")
	p("")
	p("     orch whatsapp status        # config, Graph reachability, webhook listener")
	p("     orch whatsapp test          # send yourself a message")
	p("")
	p("   Cloud API note: free-form messages only reach a number that has messaged the")
	p("   business in the last 24 hours. Send any message to the business number first;")
	p("   if `orch whatsapp test` reports the 24-hour window, that is why.")
	p("")
	p("Check the callback locally once the listener is up:")
	p("")
	q := url.Values{"hub.mode": {"subscribe"}, "hub.verify_token": {g.VerifyToken}, "hub.challenge": {"hello"}}
	p("     curl '%s%s?%s'", local, g.Webhook.Path, q.Encode())
	p("     (expect HTTP 200 and the body 'hello'; %s/health reports the listener's state)", local)
	if g.ConfigPath != "" {
		p("")
		p("To send daemon notifications to the owner, add routes to %s, e.g.:", g.ConfigPath)
		chat := "<owner-number>"
		if len(g.AllowFrom) > 0 {
			chat = g.AllowFrom[0]
		}
		p("")
		p("     routes:")
		p("       - {topic: wolf,       channel: whatsapp, chat: \"%s\"}", chat)
		p("       - {topic: workingman, channel: whatsapp, chat: \"%s\"}", chat)
	}
	if g.WABAID == "" {
		p("")
		p("Tip: pass --waba-id (App Dashboard > WhatsApp > API Setup) to make the webhook")
		p("accept only events for your own WhatsApp Business Account.")
	}
}
