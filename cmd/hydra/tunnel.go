package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/journal"
	"github.com/santiagotoscanini/hydra/internal/pc"
	"github.com/santiagotoscanini/hydra/internal/router"
	"github.com/santiagotoscanini/hydra/internal/routes"
	"github.com/santiagotoscanini/hydra/internal/tunnels"
)

// A tunnel (x-hydra tunnel) is one public address for the whole repository,
// for webhooks. It runs in main and sends its requests to a fixed port,
// where main's hydra-router passes them to the copy of its service in the
// checkout that holds the tunnel: main's, until a worktree takes it. Who
// holds it is a file in the shared .git directory (package tunnels).

// tunnelView is one tunnel and where its requests go now.
type tunnelView struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
	To   string `json:"to"`
	Port int    `json:"port"` // where the tunnel sends its requests
	// Holder is the root of the worktree that holds it, "" for main.
	Holder string     `json:"holder"`
	Branch string     `json:"branch,omitempty"`
	Since  *time.Time `json:"since,omitempty"`
	// Target is where requests go now: "fix-cart:9080" (the holder's own
	// copy), "main:8080", or "main:8080 for fix-cart" (a holder that runs no
	// copy of its own: main's, with the holder's tag).
	Target string `json:"target"`
	// Down, when set, is why requests get a 502 now.
	Down string `json:"down,omitempty"`

	port  int    // of the copy requests go to
	tag   string // the holder's worktree tag
	own   bool   // the holder's own copy
	label string // its name for messages: the worktree's folder, or "main"
}

// tunnelViews works out where each tunnel's requests go: to the holder's
// own copy of the service when it runs one, else to main's with the
// holder's tag. A holder whose stack isn't running gets every request
// refused, never sent to main: the developer moved the tunnel there to see
// them.
func tunnelViews(st *config.Stack, held map[string]tunnels.Holder, files []routes.File, live func(routes.File) bool) []tunnelView {
	var out []tunnelView
	for _, name := range st.Tunnels() {
		tn := st.Services[name].Tunnel
		mainPort := st.Services[tn.To].Port
		v := tunnelView{Name: name, URL: tn.URL, To: tn.To, Port: tn.Port, port: mainPort, label: "main",
			Target: "main:" + strconv.Itoa(mainPort)}
		h, ok := held[name]
		if !ok || h.Root == "" {
			out = append(out, v)
			continue
		}
		since := h.Since
		v.Holder, v.Branch, v.Since, v.label = h.Root, h.Branch, &since, filepath.Base(h.Root)
		var f *routes.File
		for i := range files {
			if files[i].Root == h.Root {
				f = &files[i]
			}
		}
		switch {
		case f == nil || !live(*f):
			v.Target = v.label + " (stack not running)"
			v.Down = fmt.Sprintf("worktree %s holds tunnel %s, and its stack isn't running. There: `hydra up -D`, or give the tunnel back: `hydra tunnel release %s`",
				v.label, name, name)
		default:
			v.tag = f.Tag()
			if p, ok := f.Local[tn.To]; ok {
				v.port, v.own = p, true
				v.Target = v.label + ":" + strconv.Itoa(p)
			} else {
				v.Target += " for " + v.label
			}
		}
		out = append(out, v)
	}
	return out
}

// loadTunnels is tunnelViews for the repository as it is now.
func loadTunnels(commonDir string, st *config.Stack) ([]tunnelView, error) {
	if !st.HasTunnel() {
		return nil, nil
	}
	held, err := tunnels.Read(commonDir)
	if err != nil {
		return nil, err
	}
	files, err := routes.Read(commonDir)
	if err != nil {
		return nil, err
	}
	return tunnelViews(st, held, files, routes.File.Live), nil
}

// routerTunnel is what the router needs of v.
func (v tunnelView) routerTunnel() router.Tunnel {
	t := router.Tunnel{Listen: v.Port, To: v.To, Target: v.port, Label: v.To + "@main", Tag: v.tag, Own: v.own, Down: v.Down}
	if v.Holder != "" {
		t.Holder = v.label
		if v.own {
			t.Label = v.To + "@" + v.label
		} else {
			t.Label += " for " + v.label
		}
	}
	return t
}

// title is "hooks (https://hooks.example.com)", or the name alone.
func (v tunnelView) title() string {
	if v.URL == "" {
		return v.Name
	}
	return v.Name + " (" + v.URL + ")"
}

// holderText is who holds it, for a person: "main", or "fix-cart (branch
// fix/cart) since 14:02".
func (v tunnelView) holderText() string {
	if v.Holder == "" {
		return "main"
	}
	s := v.label
	if v.Branch != "" && v.Branch != v.label {
		s += " (" + v.Branch + ")"
	}
	if v.Since != nil && !v.Since.IsZero() {
		s += " since " + sinceText(*v.Since)
	}
	return s
}

// sinceText is a time as `hydra tunnel` shows it: the clock today, the date
// before.
func sinceText(t time.Time) string {
	t = t.Local()
	if y, m, d := t.Date(); y == time.Now().Year() && m == time.Now().Month() && d == time.Now().Day() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

// tunnelCmd is `hydra tunnel [take|release] [NAME]`.
func tunnelCmd(args []string) error {
	co, err := gitx.Find(".")
	if err != nil {
		return err
	}
	st := mainStackOf(co)
	if st == nil {
		return fmt.Errorf("can't read main's process-compose.yaml (%s), where tunnels are declared", filepath.Join(co.Main, "process-compose.yaml"))
	}
	if !st.HasTunnel() {
		return fmt.Errorf("main's process-compose.yaml declares no tunnel (x-hydra.tunnel on the tunnel's process: { to: SERVICE, port: PORT })")
	}
	action, rest := "", args
	if len(args) > 0 && (args[0] == "take" || args[0] == "release") {
		action, rest = args[0], args[1:]
	}
	if len(rest) > 1 || (len(rest) == 1 && strings.HasPrefix(rest[0], "-")) {
		return fmt.Errorf("usage: hydra tunnel [take|release] [NAME]")
	}
	switch action {
	case "":
		if len(rest) > 0 {
			return fmt.Errorf("unknown tunnel command %q (hydra tunnel, hydra tunnel take [NAME], hydra tunnel release [NAME])", rest[0])
		}
		views, err := loadTunnels(co.CommonDir, st)
		if err != nil {
			return err
		}
		printTunnels(os.Stdout, views)
		return nil
	case "take":
		name, err := tunnelName(st, rest)
		if err != nil {
			return err
		}
		if co.IsMain() {
			return releaseTunnel(co, st, name, os.Stdout)
		}
		return takeTunnel(co, st, name, os.Stdout)
	default:
		name, err := tunnelName(st, rest)
		if err != nil {
			return err
		}
		return releaseTunnel(co, st, name, os.Stdout)
	}
}

// tunnelName is the tunnel named, or the only one there is.
func tunnelName(st *config.Stack, rest []string) (string, error) {
	names := st.Tunnels()
	if len(rest) == 0 {
		if len(names) == 1 {
			return names[0], nil
		}
		return "", fmt.Errorf("there are %d tunnels; name one: %s", len(names), strings.Join(names, ", "))
	}
	if s := st.Services[rest[0]]; s == nil || s.Tunnel == nil {
		return "", fmt.Errorf("no tunnel is called %q (tunnels: %s)", rest[0], strings.Join(names, ", "))
	}
	return rest[0], nil
}

func printTunnels(w io.Writer, views []tunnelView) {
	for i, v := range views {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "tunnel %s\n", v.title())
		fmt.Fprintf(w, "  reaches  %s, through :%d (hydra-router)\n", v.To, v.Port)
		fmt.Fprintf(w, "  held by  %s\n", v.holderText())
		if v.Down != "" {
			fmt.Fprintf(w, "  goes to  nowhere: %s\n", v.Down)
		} else {
			fmt.Fprintf(w, "  goes to  %s\n", v.Target)
		}
	}
}

// viewOf finds a tunnel's view.
func viewOf(co gitx.Checkout, st *config.Stack, name string) (tunnelView, error) {
	views, err := loadTunnels(co.CommonDir, st)
	if err != nil {
		return tunnelView{}, err
	}
	for _, v := range views {
		if v.Name == name {
			return v, nil
		}
	}
	return tunnelView{}, fmt.Errorf("no tunnel %s", name)
}

// takeTunnel gives the tunnel to the worktree co, and says who had it and
// where requests go now.
func takeTunnel(co gitx.Checkout, st *config.Stack, name string, w io.Writer) error {
	prev, err := tunnels.Take(co.CommonDir, name, tunnels.Holder{Root: co.Root, Branch: gitx.Branch(co.Root), Since: time.Now()})
	if err != nil {
		return err
	}
	v, err := viewOf(co, st, name)
	if err != nil {
		return err
	}
	had := "main"
	if prev.Root != "" {
		had = filepath.Base(prev.Root)
	}
	if prev.Root == co.Root {
		fmt.Fprintf(w, "tunnel %s already comes here (since %s)\n", v.title(), sinceText(prev.Since))
	} else {
		journal.Log("tunnel", journal.Fields{"name": name, "action": "take", "from": prevRoot(prev), "to": co.Root})
		fmt.Fprintf(w, "tunnel %s now comes here; %s had it\n", v.title(), had)
	}
	switch {
	case v.Down != "":
		fmt.Fprintf(w, "this worktree's stack isn't running, so its requests get a 502 until `hydra up -D` here (or `hydra tunnel release`)\n")
	case v.own:
		fmt.Fprintf(w, "its requests reach this worktree's %s on :%d\n", v.To, v.port)
	default:
		fmt.Fprintf(w, "this worktree runs no %s of its own: its requests reach main's (:%d), tagged as this worktree's, so routed services serve them here\n", v.To, v.port)
	}
	fmt.Fprintf(w, "give it back with: hydra tunnel release%s\n", nameArg(st, name))
	return nil
}

// releaseTunnel gives the tunnel back to main.
func releaseTunnel(co gitx.Checkout, st *config.Stack, name string, w io.Writer) error {
	prev, err := tunnels.Release(co.CommonDir, name)
	if err != nil {
		return err
	}
	title := name
	if u := st.Services[name].Tunnel.URL; u != "" {
		title += " (" + u + ")"
	}
	if prev.Root == "" {
		fmt.Fprintf(w, "tunnel %s already goes to main\n", title)
		return nil
	}
	journal.Log("tunnel", journal.Fields{"name": name, "action": "release", "from": prev.Root, "why": "hydra tunnel"})
	fmt.Fprintf(w, "tunnel %s goes to main's %s again; %s had it\n", title, st.Services[name].Tunnel.To, filepath.Base(prev.Root))
	return nil
}

func prevRoot(h tunnels.Holder) string {
	if h.Root == "" {
		return "main"
	}
	return h.Root
}

// nameArg is " NAME" when there is more than one tunnel to tell apart.
func nameArg(st *config.Stack, name string) string {
	if len(st.Tunnels()) > 1 {
		return " " + name
	}
	return ""
}

// releaseTunnelsOf gives back every tunnel the worktree at root holds, when
// its stack stops for good or for now (why: "hydra down", "removed", "idle"),
// says so on w (nil: only the journal), and returns their names.
func releaseTunnelsOf(commonDir, root, why string, w io.Writer) []string {
	names, err := tunnels.ReleaseRoot(commonDir, root)
	if err != nil {
		if w != nil {
			fmt.Fprintf(w, "hydra: warning: can't give %s's tunnels back to main: %v\n", filepath.Base(root), err)
		}
		return nil
	}
	for _, name := range names {
		journal.Log("tunnel", journal.Fields{"name": name, "action": "release", "from": root, "why": why})
		if w != nil {
			fmt.Fprintf(w, "hydra: tunnel %s goes to main again (%s held it)\n", name, filepath.Base(root))
		}
	}
	return names
}

// tunnelProblems is what `hydra status` says about the tunnels in a
// worktree whose stack was started: an error where the worktree holds one
// whose requests fail, a note where it runs a tunnel's service and another
// checkout gets the requests.
func tunnelProblems(ms *mainState, ws *wtState) []problem {
	views, err := ms.tunnels()
	if err != nil {
		return []problem{{Text: "can't tell who holds the tunnels: " + err.Error()}}
	}
	root := ws.target.checkout.Root
	runs := map[string]bool{}
	for _, row := range ws.Plan.Services {
		runs[row.Name] = row.Local && !row.Disabled
	}
	var out []problem
	for _, v := range views {
		back := "hydra tunnel release" + nameArg(ms.target.stack, v.Name)
		switch {
		case v.Holder == root && !ws.Up:
			out = append(out, problem{Blocks: true,
				Text: fmt.Sprintf("this worktree holds tunnel %s, and its stack isn't running: the tunnel's requests get a 502.", v.title()),
				Fix:  "hydra up -D   (or give the tunnel back: " + back + ")"})
		case v.Holder == root && v.own:
			p, ok := ws.Procs[v.To]
			if ok && (p.Health() == pc.Up || p.Health() == pc.Starting) {
				continue
			}
			state := "not in the running project"
			if ok {
				state = p.Describe()
			}
			out = append(out, problem{Blocks: true,
				Text: fmt.Sprintf("this worktree holds tunnel %s, and its %s is %s: the tunnel's requests get a 502.", v.title(), v.To, state),
				Fix:  "hydra up -D " + v.To + "   (or give the tunnel back: " + back + ")"})
		case v.Holder != root && runs[v.To]:
			who := "main's"
			if v.Holder != "" {
				who = v.label + "'s"
			}
			out = append(out, problem{
				Text: fmt.Sprintf("webhooks through tunnel %s go to %s %s, not this worktree's.", v.title(), who, v.To),
				Fix:  "hydra tunnel take" + nameArg(ms.target.stack, v.Name) + "   (only if you need them here; hydra tunnel release gives it back)"})
		}
	}
	return out
}

// printTunnelLines is the head of `hydra status`: in a worktree, the
// tunnels that come to it; in main (root ""), every tunnel and where it goes.
func printTunnelLines(ms *mainState, root string) {
	views, err := ms.tunnels()
	if err != nil {
		fmt.Printf("tunnel    can't tell who holds the tunnels: %v\n", err)
		return
	}
	for _, v := range views {
		switch {
		case root == "" && v.Holder == "":
			fmt.Printf("tunnel    %s goes to main's %s :%d\n", v.title(), v.To, v.port)
		case root == "" && v.Down != "":
			fmt.Printf("tunnel    %s is held by %s, whose stack isn't running: its requests get a 502\n", v.title(), v.holderText())
		case root == "":
			fmt.Printf("tunnel    %s is held by %s: goes to %s\n", v.title(), v.holderText(), v.Target)
		case v.Holder == root && v.own:
			fmt.Printf("tunnel    %s comes here: %s :%d\n", v.title(), v.To, v.port)
		case v.Holder == root && v.Down == "":
			fmt.Printf("tunnel    %s comes here: main's %s :%d, tagged as this worktree's\n", v.title(), v.To, v.port)
		}
	}
}
