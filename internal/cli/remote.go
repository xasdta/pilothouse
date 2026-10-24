package cli

// Workstream E: pairing phones, listing devices, opening the web app and
// exposing it on the tailnet with Tailscale Serve.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mdp/qrterminal/v3"
	"github.com/spf13/cobra"

	"github.com/xasdta/pilothouse/internal/api"
	"github.com/xasdta/pilothouse/internal/client"
	"github.com/xasdta/pilothouse/internal/config"
)

// rmtDaemon is the part of the daemon client these commands use.
type rmtDaemon interface {
	Pair(ctx context.Context) (api.PairStart, error)
	LoginLink(ctx context.Context) (api.LoginLink, error)
	Devices(ctx context.Context) ([]api.Device, error)
	RevokeDevice(ctx context.Context, id string) error
	Snapshot(ctx context.Context) (api.Snapshot, error)
	Stream(ctx context.Context, after int64, fn func(api.Event)) error
}

// Seams for tests.
var (
	rmtClient      = func() rmtDaemon { return client.New() }
	rmtOpenBrowser = rmtDefaultOpenBrowser
	rmtTailscale   = rmtTSExec{
		output: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		},
		interactive: rmtRunInteractive,
		lookPath:    exec.LookPath,
		stat:        os.Stat,
	}
	rmtNow = time.Now
)

func rmtCtx(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// rmtErr turns daemon errors into next steps.
func rmtErr(err error, what string) error {
	switch {
	case errors.Is(err, client.ErrDaemonDown):
		return err
	case client.IsCode(err, "agent_caller"):
		return fmt.Errorf("%s can't be run from inside an agent session (agents can't approve themselves) — run it in your own terminal", what)
	}
	return err
}

// ---- pilothouse pair ----

func newPairCmd() *cobra.Command {
	var noQR, noWait, asJSON bool
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "Pair a phone or browser to answer requests",
		Long: `Creates a one-time pairing code (valid 10 minutes) and shows it as a QR code.
Scan it with your phone's camera, name the device, and it can answer your
agents' permission requests — every answer is signed with a key that never
leaves the phone.

To reach this computer from your phone, run ` + "`pilothouse remote tailscale`" + ` first.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := rmtCtx(cmd)
			c := rmtClient()
			var after int64 = -1
			known := map[string]bool{}
			if !noWait && !asJSON {
				if snap, err := c.Snapshot(ctx); err == nil {
					after = snap.Seq
				}
				ds, _ := c.Devices(ctx)
				for _, d := range ds {
					known[d.ID] = true
				}
			}
			p, err := c.Pair(ctx)
			if err != nil {
				return rmtErr(err, "pilothouse pair")
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return json.NewEncoder(out).Encode(p)
			}
			rmtPrintPair(out, p, !noQR)
			if noWait || after < 0 {
				return nil
			}
			return rmtWaitForDevice(ctx, c, out, after, p.ExpiresAt, known)
		},
	}
	cmd.Flags().BoolVar(&noQR, "no-qr", false, "don't draw the QR code")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "exit right away instead of waiting for the device")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the pairing code as JSON")
	return cmd
}

func rmtPrintPair(w io.Writer, p api.PairStart, qr bool) {
	mins := int(p.ExpiresAt.Sub(rmtNow()).Round(time.Minute).Minutes())
	if !p.Remote {
		fmt.Fprintln(w, "! No remote URL is configured: this link only works in a browser on this computer.")
		fmt.Fprintln(w, "  To pair a phone, first run `pilothouse remote tailscale` (or set web.public_url in config.yaml).")
		fmt.Fprintln(w)
	}
	if qr && p.Remote {
		fmt.Fprintln(w, "Scan with your phone's camera:")
		qrterminal.GenerateWithConfig(p.URL, qrterminal.Config{
			Level:          qrterminal.L,
			Writer:         w,
			HalfBlocks:     true,
			BlackChar:      qrterminal.BLACK_BLACK,
			WhiteBlackChar: qrterminal.WHITE_BLACK,
			WhiteChar:      qrterminal.WHITE_WHITE,
			BlackWhiteChar: qrterminal.BLACK_WHITE,
			QuietZone:      2,
		})
	}
	fmt.Fprintf(w, "  Link:  %s\n", p.URL)
	fmt.Fprintf(w, "  Code:  %s  (one-time, expires in %d min at %s)\n", p.Code, max(mins, 1), p.ExpiresAt.Local().Format("15:04"))
}

// rmtWaitForDevice follows the event log until a device that wasn't
// paired before (not in known) appears, or the code expires.
func rmtWaitForDevice(ctx context.Context, c rmtDaemon, w io.Writer, after int64, until time.Time, known map[string]bool) error {
	fmt.Fprintln(w, "\nWaiting for the device… (Ctrl-C to stop)")
	ctx, cancel := context.WithDeadline(ctx, until)
	defer cancel()
	var paired *api.Device
	// Stream errors are ignored: pairing doesn't depend on watching it.
	_ = c.Stream(ctx, after, func(ev api.Event) {
		if ev.Type != api.EventDeviceChanged || paired != nil {
			return
		}
		var d api.Device
		if json.Unmarshal(ev.Data, &d) == nil && d.RevokedAt == nil && !known[d.ID] {
			paired = &d
			cancel()
		}
	})
	switch {
	case paired != nil:
		fmt.Fprintf(w, "✓ Paired %q (%s). Turn on notifications in its Settings tab.\n", paired.Name, paired.ID)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		fmt.Fprintln(w, "The code expired. Run `pilothouse pair` again.")
	}
	return nil
}

// ---- pilothouse devices ----

func newDevicesCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "devices",
		Short: "List paired phones and browsers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ds, err := rmtClient().Devices(rmtCtx(cmd))
			if err != nil {
				return rmtErr(err, "pilothouse devices")
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return json.NewEncoder(out).Encode(ds)
			}
			if len(ds) == 0 {
				fmt.Fprintln(out, "No paired devices. Pair your phone with `pilothouse pair`.")
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tNOTIFICATIONS\tPAIRED\tLAST SEEN")
			for _, d := range ds {
				push := "off"
				if d.PushOK {
					push = "on"
				}
				seen := "never"
				if d.LastSeenAt != nil {
					seen = rmtAgo(*d.LastSeenAt)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.ID, d.Name, push, d.CreatedAt.Local().Format("2006-01-02"), seen)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.AddCommand(&cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke a paired device (it can no longer see or answer requests)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := rmtCtx(cmd)
			c := rmtClient()
			ds, err := c.Devices(ctx)
			if err != nil {
				return rmtErr(err, "pilothouse devices revoke")
			}
			var match []api.Device
			for _, d := range ds {
				if d.ID == args[0] {
					match = []api.Device{d}
					break
				}
				if strings.HasPrefix(d.ID, args[0]) {
					match = append(match, d)
				}
			}
			switch len(match) {
			case 0:
				return fmt.Errorf("no paired device %q (see `pilothouse devices`)", args[0])
			case 1:
			default:
				return fmt.Errorf("%q matches %d devices; use more of the id", args[0], len(match))
			}
			if err := c.RevokeDevice(ctx, match[0].ID); err != nil {
				return rmtErr(err, "pilothouse devices revoke")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Revoked %q (%s).\n", match[0].Name, match[0].ID)
			return nil
		},
	})
	return cmd
}

func rmtAgo(t time.Time) string {
	d := rmtNow().Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// ---- pilothouse open ----

func newOpenCmd() *cobra.Command {
	var printOnly bool
	cmd := &cobra.Command{
		Use:   "open",
		Short: "Open the web app in this computer's browser (signs it in)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := rmtClient().LoginLink(rmtCtx(cmd))
			if err != nil {
				return rmtErr(err, "pilothouse open")
			}
			out := cmd.OutOrStdout()
			if printOnly {
				fmt.Fprintln(out, l.URL)
				return nil
			}
			if err := rmtOpenBrowser(l.URL); err != nil {
				fmt.Fprintf(out, "Couldn't open a browser (%v). Open this one-time link yourself:\n  %s\n", err, l.URL)
				return nil
			}
			fmt.Fprintln(out, "Opened the Pilothouse web app in your browser.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&printOnly, "print", false, "print the one-time link instead of opening it")
	return cmd
}

func rmtDefaultOpenBrowser(url string) error {
	var name string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	default:
		name = "xdg-open"
	}
	return exec.Command(name, url).Start()
}

// ---- pilothouse remote ----

// rmtTSExec runs the tailscale CLI.
type rmtTSExec struct {
	output      func(ctx context.Context, name string, args ...string) ([]byte, error)
	interactive func(ctx context.Context, name string, args ...string) error
	lookPath    func(string) (string, error)
	stat        func(string) (os.FileInfo, error)
}

// rmtMacTailscale is the CLI inside the macOS app (App Store and standalone).
const rmtMacTailscale = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

func rmtRunInteractive(ctx context.Context, name string, args ...string) error {
	c := exec.CommandContext(ctx, name, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

func (t rmtTSExec) binary() (string, error) {
	if p, err := t.lookPath("tailscale"); err == nil {
		return p, nil
	}
	if _, err := t.stat(rmtMacTailscale); err == nil {
		return rmtMacTailscale, nil
	}
	return "", errors.New("tailscale is not installed — get it from https://tailscale.com/download, sign in, then run `pilothouse remote tailscale` again")
}

// rmtTSStatus is the subset of `tailscale status --json` we read.
type rmtTSStatus struct {
	BackendState string   `json:"BackendState"`
	CertDomains  []string `json:"CertDomains"`
	Self         struct {
		DNSName string `json:"DNSName"`
		Online  bool   `json:"Online"`
	} `json:"Self"`
	CurrentTailnet *struct {
		Name            string `json:"Name"`
		MagicDNSEnabled bool   `json:"MagicDNSEnabled"`
	} `json:"CurrentTailnet"`
}

func (t rmtTSExec) status(ctx context.Context, bin string) (rmtTSStatus, error) {
	var st rmtTSStatus
	out, err := t.output(ctx, bin, "status", "--json")
	if err != nil && len(out) == 0 {
		return st, fmt.Errorf("`tailscale status` failed: %v — is Tailscale running?", err)
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return st, fmt.Errorf("couldn't read `tailscale status --json`: %v", err)
	}
	return st, nil
}

// rmtServeArgs is the Tailscale Serve command for the web listener:
// HTTPS on 443 of this machine's tailnet name → http://127.0.0.1:<port>,
// persistent (--bg) across restarts. https://tailscale.com/kb/1242/tailscale-serve
func rmtServeArgs(port string) []string {
	return []string{"serve", "--bg", "--https=443", "http://127.0.0.1:" + port}
}

func rmtServeOffArgs() []string { return []string{"serve", "--https=443", "off"} }

func rmtWebPort(cfg *config.Config) string {
	if _, p, err := net.SplitHostPort(cfg.Web.Addr); err == nil && p != "" {
		return p
	}
	return "7517"
}

func newRemoteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "Reach the web app from your phone (Tailscale Serve)",
		Long: `The web app listens on 127.0.0.1 only. ` + "`pilothouse remote tailscale`" + ` publishes it to
your tailnet over HTTPS with Tailscale Serve, so your phone (signed in to the
same tailnet) can reach it. Nothing is exposed to the public internet.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "tailscale",
		Short: "Serve the web app on your tailnet over HTTPS",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return rmtTailscaleOn(rmtCtx(cmd), cmd.OutOrStdout()) },
	}, &cobra.Command{
		Use:   "off",
		Short: "Stop serving the web app on your tailnet",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return rmtTailscaleOff(rmtCtx(cmd), cmd.OutOrStdout()) },
	}, &cobra.Command{
		Use:   "status",
		Short: "Show how the web app is reachable",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return rmtStatus(rmtCtx(cmd), cmd.OutOrStdout()) },
	})
	return cmd
}

func rmtTailscaleOn(ctx context.Context, w io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	bin, err := rmtTailscale.binary()
	if err != nil {
		return err
	}
	st, err := rmtTailscale.status(ctx, bin)
	if err != nil {
		return err
	}
	if st.BackendState != "Running" {
		return fmt.Errorf("tailscale is %s, not connected — run `tailscale up` (or open the Tailscale app and sign in), then try again", rmtOr(st.BackendState, "stopped"))
	}
	host := strings.TrimSuffix(st.Self.DNSName, ".")
	if host == "" || (st.CurrentTailnet != nil && !st.CurrentTailnet.MagicDNSEnabled) {
		return errors.New("MagicDNS is off for your tailnet, so this machine has no HTTPS name.\n" +
			"Turn it on at https://login.tailscale.com/admin/dns (MagicDNS → Enable), then run `pilothouse remote tailscale` again")
	}
	if len(st.CertDomains) == 0 {
		return errors.New("HTTPS certificates are off for your tailnet (phones need HTTPS for notifications and secure keys).\n" +
			"Enable them at https://login.tailscale.com/admin/dns → HTTPS Certificates → Enable HTTPS, then run `pilothouse remote tailscale` again")
	}
	port := rmtWebPort(cfg)
	fmt.Fprintf(w, "Starting Tailscale Serve: https://%s → http://127.0.0.1:%s\n", host, port)
	if err := rmtTailscale.interactive(ctx, bin, rmtServeArgs(port)...); err != nil {
		return fmt.Errorf("`tailscale %s` failed: %v\n(If Tailscale asked you to enable Serve for your tailnet, follow its link and run this again.)", strings.Join(rmtServeArgs(port), " "), err)
	}
	// `tailscale serve` may have waited on the person: re-read and change
	// only public_url, keeping whatever was saved meanwhile (the daemon
	// pairing a Telegram chat, another command).
	if cfg, err = config.Update(nil, func(c *config.Config) error {
		c.Web.PublicURL = "https://" + host
		return nil
	}); err != nil {
		return fmt.Errorf("tailscale serve is running but saving web.public_url failed: %w", err)
	}
	fmt.Fprintf(w, "\n✓ The web app is on your tailnet at %s\n", cfg.Web.PublicURL)
	fmt.Fprintln(w, "  Only devices signed in to your tailnet can reach it. The first visit may take a few seconds while the certificate is issued.")
	fmt.Fprintln(w, "\nNext: run `pilothouse pair` and scan the QR code with your phone.")
	return nil
}

func rmtTailscaleOff(ctx context.Context, w io.Writer) error {
	if _, err := config.Load(); err != nil {
		return err
	}
	if bin, err := rmtTailscale.binary(); err == nil {
		if err := rmtTailscale.interactive(ctx, bin, rmtServeOffArgs()...); err != nil {
			fmt.Fprintf(w, "! `tailscale %s` failed (%v); check `tailscale serve status`.\n", strings.Join(rmtServeOffArgs(), " "), err)
		}
	}
	had := ""
	if _, err := config.Update(nil, func(c *config.Config) error {
		had, c.Web.PublicURL = c.Web.PublicURL, ""
		return nil
	}); err != nil {
		return err
	}
	if had != "" {
		fmt.Fprintf(w, "✓ Stopped serving %s. Paired phones can't reach this computer until you run `pilothouse remote tailscale` again.\n", had)
	} else {
		fmt.Fprintln(w, "✓ Remote access is off.")
	}
	return nil
}

func rmtStatus(ctx context.Context, w io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Local:   http://%s\n", cfg.Web.Addr)
	if cfg.Web.PublicURL == "" {
		fmt.Fprintln(w, "Remote:  off — run `pilothouse remote tailscale` to reach the web app from your phone")
	} else {
		fmt.Fprintf(w, "Remote:  %s\n", cfg.Web.PublicURL)
	}
	bin, err := rmtTailscale.binary()
	if err != nil {
		fmt.Fprintln(w, "Tailscale: not installed")
		return nil
	}
	out, err := rmtTailscale.output(ctx, bin, "serve", "status")
	if err != nil && len(out) == 0 {
		fmt.Fprintf(w, "Tailscale: `tailscale serve status` failed: %v\n", err)
		return nil
	}
	fmt.Fprintf(w, "\nTailscale Serve:\n%s", out)
	if s := string(out); cfg.Web.PublicURL != "" && !strings.Contains(s, "127.0.0.1:"+rmtWebPort(cfg)) {
		fmt.Fprintln(w, "\n! Tailscale Serve isn't proxying to Pilothouse — run `pilothouse remote tailscale` again.")
	}
	return nil
}

func rmtOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
