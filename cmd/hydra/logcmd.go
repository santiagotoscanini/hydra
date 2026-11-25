package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/journal"
)

// logCmd prints the journal: what hydra did and saw in main and every
// worktree of this repository.
func logCmd(args []string) error {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	since := fs.Duration("since", time.Hour, "")
	here := fs.Bool("here", false, "")
	errs := fs.Bool("errors", false, "")
	asJSON := fs.Bool("json", false, "")
	follow := fs.Bool("f", false, "")
	fs.BoolVar(follow, "follow", false, "")
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return err
	}
	co, err := gitx.Find(".")
	if err != nil {
		return err
	}
	kinds := map[string]bool{}
	for _, k := range fs.Args() {
		kinds[k] = true
	}
	keep := func(r journal.Record) bool {
		return (!*here || r.Checkout == checkoutName(co)) && (len(kinds) == 0 || kinds[r.Kind]) && (!*errs || bad(r))
	}
	from := time.Now().Add(-*since)
	var last time.Time
	printed := map[string]bool{} // lines already shown while following
	for {
		recs, err := journal.Read(co.CommonDir, from)
		if err != nil {
			return err
		}
		days := spansDays(recs)
		for _, r := range recs {
			if !keep(r) || (*follow && printed[string(r.Line)]) {
				continue
			}
			if *follow {
				printed[string(r.Line)] = true
			}
			if *asJSON {
				os.Stdout.Write(r.Line)
			} else {
				fmt.Println(formatRecord(r, days))
			}
			last = r.Time
		}
		if !*follow {
			return nil
		}
		if !last.IsZero() {
			from = last
		}
		time.Sleep(time.Second)
	}
}

// reorderFlags lets flags follow the kinds (`hydra log process --since 2h`).
func reorderFlags(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			continue
		}
		flags = append(flags, a)
		if a == "--since" || a == "-since" || a == "-o" || a == "--out" {
			if i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
		}
	}
	return append(flags, rest...)
}

func spansDays(recs []journal.Record) bool {
	return len(recs) > 0 && recs[0].Time.Format("2006-01-02") != recs[len(recs)-1].Time.Format("2006-01-02")
}

// bad says whether an event is something going wrong.
func bad(r journal.Record) bool {
	switch r.Kind {
	case "command":
		return r.Num("exit") != 0
	case "problem":
		return r.Str("level") == "error"
	case "process":
		return r.Str("health") == "failed" || r.Raw["output"] != nil
	case "idle":
		return r.Str("error") != ""
	case "log":
		m := strings.ToLower(r.Str("msg"))
		return strings.Contains(m, "fail") || strings.Contains(m, "error") || strings.Contains(m, "can't") || strings.Contains(m, "didn't")
	}
	return false
}

func formatRecord(r journal.Record, days bool) string {
	when := r.Time.Format("15:04:05")
	if days {
		when = r.Time.Format("01-02 15:04:05")
	}
	who := r.Checkout
	if who == "" {
		who = "-"
	}
	if r.Slot > 0 {
		who += " [" + strconv.Itoa(r.Slot) + "]"
	}
	return fmt.Sprintf("%s  %-28s %-8s %s", when, trunc(who, 28), r.Kind, summary(r))
}

func summary(r journal.Record) string {
	switch r.Kind {
	case "command":
		s := "hydra " + strings.Join(strs(r.Raw["args"]), " ")
		s += fmt.Sprintf("   exit %d, %s", int(r.Num("exit")), time.Duration(r.Num("ms"))*time.Millisecond)
		if e := r.Str("error"); e != "" {
			s += "\n" + logIndent(e)
		}
		return s
	case "problem":
		mark := map[string]string{"error": "✗", "warning": "!", "waiting": "…"}[r.Str("level")]
		s := mark + " " + r.Str("text")
		if f := r.Str("fix"); f != "" {
			s += "\n  fix: " + f
		}
		return indentAfterFirst(s)
	case "process":
		s := r.Str("name") + ": " + r.Str("status")
		if w := r.Str("was"); w != "" && w != r.Str("status") {
			s = r.Str("name") + ": " + w + " → " + r.Str("status")
		}
		if h := r.Str("health"); h != "" {
			s += " (" + h + ")"
		}
		if n := r.Num("exit"); n != 0 {
			s += fmt.Sprintf(", exit %d", int(n))
		}
		if n := r.Num("restarts"); n != 0 {
			s += fmt.Sprintf(", restarted %d×", int(n))
		}
		if r.Raw["first_look"] == true {
			s += "   (as first seen)"
		}
		if out := strs(r.Raw["output"]); len(out) > 0 {
			if len(out) > 8 {
				out = out[len(out)-8:]
			}
			s += "\n" + logIndent(strings.Join(out, "\n"))
		}
		return s
	case "usage":
		s := fmt.Sprintf("%.0f MB, %.0f%% CPU", r.Num("mb"), r.Num("cpu"))
		if procs, ok := r.Raw["procs"].(map[string]any); ok {
			type p struct {
				name string
				mb   float64
			}
			var ps []p
			for name, v := range procs {
				m, _ := v.(map[string]any)
				mb, _ := m["mb"].(float64)
				ps = append(ps, p{name, mb})
			}
			sort.Slice(ps, func(i, j int) bool { return ps[i].mb > ps[j].mb })
			var top []string
			for i := 0; i < len(ps) && i < 3; i++ {
				top = append(top, fmt.Sprintf("%s %.0f MB", ps[i].name, ps[i].mb))
			}
			if len(top) > 0 {
				s += " (" + strings.Join(top, ", ") + ")"
			}
		}
		if v, ok := r.Raw["machine_free_pct"].(float64); ok {
			s += fmt.Sprintf("; machine %d%% free", int(v))
		}
		if v, ok := r.Raw["swap_used_mb"].(float64); ok {
			s += fmt.Sprintf(", swap %.0f MB", v)
		}
		return s
	case "slot":
		if from := r.Str("took_from"); from != "" {
			return fmt.Sprintf("took slot %d from %s", int(r.Num("slot")), filepath.Base(from))
		}
		return fmt.Sprintf("slot %d", int(r.Num("slot")))
	case "start":
		return r.Str("what") + " started"
	case "idle":
		if e := r.Str("error"); e != "" {
			return "stopping its stack after " + r.Str("after") + " idle failed: " + e
		}
		return "stopped its stack after " + r.Str("after") + " idle (idle_stop)"
	case "log":
		return indentAfterFirst(r.Str("msg"))
	}
	return string(r.Line)
}

func strs(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

func logIndent(s string) string {
	return "      | " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n      | ")
}

func indentAfterFirst(s string) string { return strings.ReplaceAll(s, "\n", "\n      ") }

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// reportCmd writes one Markdown file with what is needed to understand a
// problem without access to this machine: versions, the state of main and
// every worktree now, what each used over time, what went wrong, and the
// journal itself.
func reportCmd(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	since := fs.Duration("since", 24*time.Hour, "")
	out := fs.String("o", "", "")
	if err := fs.Parse(reorderFlags(args)); err != nil {
		return err
	}
	co, err := gitx.Find(".")
	if err != nil {
		return err
	}
	from := time.Now().Add(-*since)
	recs, err := journal.Read(co.CommonDir, from)
	if err != nil {
		return err
	}
	if *out == "" {
		*out = filepath.Join(journal.Dir(co.CommonDir), "reports", "hydra-report-"+time.Now().Format("20060102-150405")+".md")
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	writeReport(f, co, recs, from)
	fmt.Println(*out)
	return nil
}

func writeReport(w io.Writer, co gitx.Checkout, recs []journal.Record, from time.Time) {
	p := func(format string, a ...any) { fmt.Fprintf(w, format, a...) }
	p("# hydra report\n\n")
	p("Written %s from %s, covering the journal since %s.\n\n", time.Now().Format(time.RFC3339), co.Root, from.Format(time.RFC3339))

	p("## Versions\n\n")
	p("- hydra %s\n", buildVersion())
	p("- process-compose %s\n", firstLine(runOut(co.Main, 10*time.Second, "process-compose", "version", "--short"), "not found"))
	p("- %s %s, %s\n", runtime.GOOS, firstLine(runOut("", 5*time.Second, "sw_vers", "-productVersion"), "?"), runtime.GOARCH)
	if mem := firstLine(runOut("", 5*time.Second, "sysctl", "-n", "hw.memsize"), ""); mem != "" {
		if n, err := strconv.ParseFloat(mem, 64); err == nil {
			p("- %.0f GB of memory", n/(1<<30))
		}
	}
	m := machineUsage()
	if v, ok := m["machine_free_pct"]; ok {
		p(", %v%% free now", v)
	}
	if v, ok := m["swap_used_mb"]; ok {
		p(", %v MB of swap in use", v)
	}
	p("\n- supabase %s\n\n", firstLine(runOut("", 10*time.Second, "supabase", "--version"), "not found"))

	self, _ := os.Executable()
	section := func(title, dir string, args ...string) {
		p("## %s\n\n```\n%s```\n\n", title, runOut(dir, 2*time.Minute, self, args...))
	}
	section("`hydra status --all` (in main)", co.Main, "status", "--all")
	if !co.IsMain() {
		section("`hydra status` (in "+filepath.Base(co.Root)+")", co.Root, "status")
	}
	section("`hydra slots`", co.Main, "slots")

	reportUsage(w, recs)
	reportCommands(w, recs)
	reportErrors(w, recs)

	p("## Journal\n\nEvery event since %s except the minute-by-minute `usage` ones (summarised above), newest last", from.Format("Jan 2 15:04"))
	var lines [][]byte
	for _, r := range recs {
		if r.Kind != "usage" {
			lines = append(lines, r.Line)
		}
	}
	if len(lines) > 5000 {
		p("; the last 5000 of %d", len(lines))
		lines = lines[len(lines)-5000:]
	}
	p(". The full journal is in `%s`.\n\n```jsonl\n", journal.Dir(co.CommonDir))
	for _, l := range lines {
		w.Write(l)
	}
	p("```\n")
}

func reportUsage(w io.Writer, recs []journal.Record) {
	type agg struct {
		n                    int
		first, last          time.Time
		mbSum, mbMax, cpuSum float64
		cpuMax               float64
	}
	by := map[string]*agg{}
	type procPeak struct {
		who, name string
		mb, cpu   float64
	}
	peaks := map[string]*procPeak{}
	minFree, maxSwap := 101.0, -1.0
	for _, r := range recs {
		if r.Kind != "usage" {
			continue
		}
		a := by[r.Checkout]
		if a == nil {
			a = &agg{first: r.Time}
			by[r.Checkout] = a
		}
		a.n++
		a.last = r.Time
		mb, cpu := r.Num("mb"), r.Num("cpu")
		a.mbSum += mb
		a.cpuSum += cpu
		a.mbMax = max(a.mbMax, mb)
		a.cpuMax = max(a.cpuMax, cpu)
		if procs, ok := r.Raw["procs"].(map[string]any); ok {
			for name, v := range procs {
				u, _ := v.(map[string]any)
				pmb, _ := u["mb"].(float64)
				pcpu, _ := u["cpu"].(float64)
				key := r.Checkout + "\x00" + name
				pk := peaks[key]
				if pk == nil {
					pk = &procPeak{who: r.Checkout, name: name}
					peaks[key] = pk
				}
				pk.mb = max(pk.mb, pmb)
				pk.cpu = max(pk.cpu, pcpu)
			}
		}
		if v, ok := r.Raw["machine_free_pct"].(float64); ok {
			minFree = min(minFree, v)
		}
		if v, ok := r.Raw["swap_used_mb"].(float64); ok {
			maxSwap = max(maxSwap, v)
		}
	}
	fmt.Fprintf(w, "## Usage\n\nSampled once a minute while a stack runs: resident memory of each process and its children (it counts mapped files, so `next dev` reads high) and CPU in percent of one core.\n\n")
	if len(by) == 0 {
		fmt.Fprintf(w, "No samples in this window.\n\n")
		return
	}
	if minFree <= 100 {
		fmt.Fprintf(w, "Machine: lowest free memory %.0f%%", minFree)
		if maxSwap >= 0 {
			fmt.Fprintf(w, ", most swap in use %.0f MB", maxSwap)
		}
		fmt.Fprintf(w, ".\n\n")
	}
	names := make([]string, 0, len(by))
	for k := range by {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool { return by[names[i]].mbMax > by[names[j]].mbMax })
	fmt.Fprintf(w, "| Checkout | Up (≈ minutes) | First | Last | Avg MB | Peak MB | Avg CPU %% | Peak CPU %% |\n|---|---|---|---|---|---|---|---|\n")
	for _, k := range names {
		a := by[k]
		fmt.Fprintf(w, "| %s | %d | %s | %s | %.0f | %.0f | %.0f | %.0f |\n", k, a.n, a.first.Format("Jan 2 15:04"), a.last.Format("Jan 2 15:04"), a.mbSum/float64(a.n), a.mbMax, a.cpuSum/float64(a.n), a.cpuMax)
	}
	var ps []*procPeak
	for _, pk := range peaks {
		ps = append(ps, pk)
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].mb > ps[j].mb })
	if len(ps) > 15 {
		ps = ps[:15]
	}
	fmt.Fprintf(w, "\nHeaviest processes (peak):\n\n| Checkout | Process | Peak MB | Peak CPU %% |\n|---|---|---|---|\n")
	for _, pk := range ps {
		fmt.Fprintf(w, "| %s | %s | %.0f | %.0f |\n", pk.who, pk.name, pk.mb, pk.cpu)
	}
	fmt.Fprintln(w)
}

func reportCommands(w io.Writer, recs []journal.Record) {
	type agg struct {
		n, failed int
		ms        float64
		maxMs     float64
	}
	by := map[string]*agg{}
	for _, r := range recs {
		if r.Kind != "command" {
			continue
		}
		args := strs(r.Raw["args"])
		name := "hydra"
		for _, a := range args {
			if strings.HasPrefix(a, "-") {
				continue
			}
			name += " " + a
			if a != "process" && a != "slots" {
				break
			}
		}
		a := by[name]
		if a == nil {
			a = &agg{}
			by[name] = a
		}
		a.n++
		if r.Num("exit") != 0 {
			a.failed++
		}
		a.ms += r.Num("ms")
		a.maxMs = max(a.maxMs, r.Num("ms"))
	}
	if len(by) == 0 {
		return
	}
	names := make([]string, 0, len(by))
	for k := range by {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool { return by[names[i]].n > by[names[j]].n })
	fmt.Fprintf(w, "## Commands\n\n| Command | Runs | Exited non-zero | Avg | Longest |\n|---|---|---|---|---|\n")
	for _, k := range names {
		a := by[k]
		fmt.Fprintf(w, "| `%s` | %d | %d | %s | %s |\n", k, a.n, a.failed, dur(a.ms/float64(a.n)), dur(a.maxMs))
	}
	fmt.Fprintln(w)
}

func dur(ms float64) string {
	return (time.Duration(ms) * time.Millisecond).Round(100 * time.Millisecond).String()
}

func reportErrors(w io.Writer, recs []journal.Record) {
	type agg struct {
		n           int
		first, last time.Time
		who         map[string]bool
		sample      journal.Record
	}
	by := map[string]*agg{}
	var order []string
	for _, r := range recs {
		if !bad(r) {
			continue
		}
		key := r.Kind + ": " + firstLine(summary(r), "")
		if r.Kind == "process" {
			key = "process: " + r.Str("name") + " " + r.Str("health")
		}
		a := by[key]
		if a == nil {
			a = &agg{first: r.Time, who: map[string]bool{}}
			by[key] = a
			order = append(order, key)
		}
		a.n++
		a.last = r.Time
		a.who[r.Checkout] = true
		a.sample = r
	}
	fmt.Fprintf(w, "## What went wrong\n\n")
	if len(order) == 0 {
		fmt.Fprintf(w, "Nothing: no failed command, error, or failed process in this window.\n\n")
		return
	}
	sort.SliceStable(order, func(i, j int) bool { return by[order[i]].last.After(by[order[j]].last) })
	for _, k := range order {
		a := by[k]
		var who []string
		for c := range a.who {
			who = append(who, c)
		}
		sort.Strings(who)
		fmt.Fprintf(w, "### %s\n\n%d× between %s and %s, in %s. The last one:\n\n```\n%s\n```\n\n", trunc(k, 140), a.n, a.first.Format("Jan 2 15:04:05"), a.last.Format("Jan 2 15:04:05"), strings.Join(who, ", "), formatRecord(a.sample, true))
	}
}

// runOut runs a command for the report and returns what it printed, with
// how it ended when that wasn't well.
func runOut(dir string, timeout time.Duration, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HYDRA_JOURNAL=off", "NO_COLOR=1")
	out, err := cmd.CombinedOutput()
	s := string(out)
	if err != nil {
		s += "(" + err.Error() + ")\n"
	}
	return s
}

func firstLine(s, otherwise string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return otherwise
	}
	line, _, _ := strings.Cut(s, "\n")
	return line
}
