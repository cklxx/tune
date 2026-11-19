package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cklxx/tune/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// The pool probes every configured host through the system ssh binary rather
// than tn's Go SSH client: some pooled hosts accept GSSAPI only, which
// golang.org/x/crypto/ssh cannot speak. Cold GSSAPI handshakes cost seconds,
// so the per-probe timeout is 20s; ControlMaster in ~/.ssh/config keeps warm
// probes near 0.6s.
//
// ponytail: system-ssh probing costs ~150ms more than a native dial and cannot
// reuse tn's held connection. Switch hosts that have a key to sshx.Dial if the
// refresh cadence ever needs to drop below one second.

type poolState struct {
	Name   string `json:"name"`
	Addr   string `json:"addr"`
	User   string `json:"user"`
	Up     bool   `json:"up"`
	RTTMs  int64  `json:"rttMs"`
	Detail string `json:"detail,omitempty"`

	CPU, Mem, Disk, GPU int // percent, -1 when unknown

	NCPU        int     `json:"ncpu,omitempty"`
	Load        float64 `json:"load1,omitempty"`
	MemUsedKB   int64   `json:"memUsedKb,omitempty"`
	MemTotalKB  int64   `json:"memTotalKb,omitempty"`
	DiskUsedKB  int64   `json:"diskUsedKb,omitempty"`
	DiskTotalKB int64   `json:"diskTotalKb,omitempty"`
	GPUUsedMB   int     `json:"gpuMemUsedMb,omitempty"`
	GPUTotalMB  int     `json:"gpuMemTotalMb,omitempty"`
}

// metricsScript runs on the remote in one round trip. SSH handshakes here cost
// seconds (GSSAPI), so four separate probes would quadruple the refresh time.
// It is base64-encoded before sending: the jump server's shell sees no quotes,
// which keeps the bastion path working with the same command.
//
// CPU is load1/ncpu, not a sampling delta: a true utilisation needs two
// /proc/stat reads and a sleep, which would add that sleep to every refresh.
const metricsScript = `LC_ALL=C
load=""; ncpu=""
if [ -r /proc/loadavg ]; then
  load=$(cut -d' ' -f1 /proc/loadavg)
  ncpu=$(nproc 2>/dev/null || grep -c '^processor' /proc/cpuinfo)
fi
memu=""; memt=""
if [ -r /proc/meminfo ]; then
  set -- $(awk '/^MemTotal:/{t=$2}/^MemAvailable:/{a=$2}END{print t, t-a}' /proc/meminfo)
  memt=$1; memu=$2
fi
set -- $(df -P / 2>/dev/null | awk 'NR==2{print $3, $2}')
disku=$1; diskt=$2
gpu=""; gpuu=""; gput=""
if command -v nvidia-smi >/dev/null 2>&1; then
  nv=$(nvidia-smi --query-gpu=utilization.gpu,memory.used,memory.total --format=csv,noheader,nounits 2>/dev/null)
  if [ -n "$nv" ]; then
    gpu=$(echo "$nv" | awk -F, '{s+=$1;c++}END{if(c)printf "%d",s/c}')
    gpuu=$(echo "$nv" | awk -F, '{s+=$2}END{printf "%d",s}')
    gput=$(echo "$nv" | awk -F, '{s+=$3}END{printf "%d",s}')
  fi
fi
echo "TN_METRIC load=$load ncpu=$ncpu memu=$memu memt=$memt disku=$disku diskt=$diskt gpu=$gpu gpuu=$gpuu gput=$gput"
`

func metricsCommand() string {
	return "echo " + base64.StdEncoding.EncodeToString([]byte(metricsScript)) + " | base64 -d | sh"
}

// parseMetrics fills the utilisation fields. A missing value stays -1 for the
// percentage and 0 for the absolute number, so the renderer can tell "no GPU"
// from "GPU idle".
func parseMetrics(out string, st *poolState) {
	st.CPU, st.Mem, st.Disk, st.GPU = -1, -1, -1, -1
	for _, ln := range strings.Split(out, "\n") {
		if !strings.HasPrefix(ln, "TN_METRIC") {
			continue
		}
		for _, f := range strings.Fields(ln)[1:] {
			k, v, ok := strings.Cut(f, "=")
			if !ok || v == "" {
				continue
			}
			switch k {
			case "load":
				if x, err := strconv.ParseFloat(v, 64); err == nil {
					st.Load = x
				}
			case "ncpu":
				st.NCPU = atoiMin(v, 0)
			case "memu":
				st.MemUsedKB = atoi64(v)
			case "memt":
				st.MemTotalKB = atoi64(v)
			case "disku":
				st.DiskUsedKB = atoi64(v)
			case "diskt":
				st.DiskTotalKB = atoi64(v)
			case "gpu":
				if n, err := strconv.Atoi(v); err == nil {
					st.GPU = clampPct(n)
				}
			case "gpuu":
				st.GPUUsedMB = atoiMin(v, 0)
			case "gput":
				st.GPUTotalMB = atoiMin(v, 0)
			}
		}
	}
	if st.NCPU > 0 {
		st.CPU = clampPct(int(st.Load*100/float64(st.NCPU) + 0.5))
	}
	if st.MemTotalKB > 0 {
		st.Mem = clampPct(int(st.MemUsedKB * 100 / st.MemTotalKB))
	}
	if st.DiskTotalKB > 0 {
		st.Disk = clampPct(int(st.DiskUsedKB * 100 / st.DiskTotalKB))
	}
}

func atoiMin(s string, min int) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < min {
		return min
	}
	return n
}

func atoi64(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func clampPct(n int) int {
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

var (
	boardInterval = 5 * time.Second
	boardProbeTO  = 20 * time.Second
	poolJSON      bool
	poolAll       bool
)

var poolCmd = &cobra.Command{
	Use:   "pool",
	Short: "Probe every configured host once and print the pool",
	Long: `One-shot version of "tn board". Hidden hosts (tag: hidden) are skipped
unless --all is given. --json emits the same data for scripts and agents.

Each row shows state, RTT, and CPU / memory / disk / GPU utilisation as a
percentage plus the absolute figure (load over core count, used over total).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		states := probePool(loadPool(poolAll))
		if poolJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(states)
		}
		fmt.Print(renderPool(states, -1, 0, 0, term.IsTerminal(int(os.Stdout.Fd()))))
		return nil
	},
}

func init() {
	poolCmd.Flags().BoolVar(&poolJSON, "json", false, "machine-readable output")
	poolCmd.Flags().BoolVar(&poolAll, "all", false, "include hidden hosts")
}

// loadPool resolves the host list from tn config, dropping entries with no
// address. Sorting is by name so rows do not move between refreshes.
func loadPool(includeHidden bool) []config.Host {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tn pool:", err)
		return nil
	}
	out := make([]config.Host, 0, len(cfg.Hosts))
	for _, h := range cfg.Hosts {
		if h == nil || h.Target.Addr == "" {
			continue
		}
		if h.Hidden && !includeHidden {
			continue
		}
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func probePool(hosts []config.Host) []poolState {
	states := make([]poolState, len(hosts))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h config.Host) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			states[i] = probePoolHost(h)
		}(i, h)
	}
	wg.Wait()
	return states
}

// sshDest splits a configured addr into the arguments the system ssh binary
// takes. ssh does not accept "host:port" as a destination, so the port goes to
// -p. IPv6 literals keep their brackets.
func sshDest(h config.Host) (dest string, port string) {
	hostport := config.EnsureAddrPort(h.Target.Addr)
	host, p, err := net.SplitHostPort(hostport)
	if err != nil {
		host, p = hostport, "22"
	}
	if h.Target.User != "" {
		host = h.Target.User + "@" + host
	}
	return host, p
}

// sshAlias reports whether ~/.ssh/config already has a Host block for this
// entry whose hostname, port and user match the tn config. When it does, the
// alias is the better handle: it carries ProxyJump, ControlMaster, GSSAPI and
// every other option the user already tuned, and a hand-built "-J user@host"
// does not reproduce them.
func sshAlias(h config.Host) (string, bool) {
	if h.Name == "" || strings.ContainsAny(h.Name, "/@ ") {
		return "", false
	}
	out, err := exec.Command("/usr/bin/ssh", "-G", h.Name).Output()
	if err != nil {
		return "", false
	}
	host, port, err := net.SplitHostPort(config.EnsureAddrPort(h.Target.Addr))
	if err != nil {
		return "", false
	}
	var gotHost, gotPort, gotUser string
	for _, ln := range strings.Split(string(out), "\n") {
		f := strings.Fields(ln)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "hostname":
			gotHost = f[1]
		case "port":
			gotPort = f[1]
		case "user":
			gotUser = f[1]
		}
	}
	if gotHost != host || gotPort != port {
		return "", false
	}
	if h.Target.User != "" && gotUser != h.Target.User {
		return "", false
	}
	return h.Name, true
}

func sshArgs(h config.Host) []string {
	if alias, ok := sshAlias(h); ok {
		return []string{alias}
	}
	dest, port := sshDest(h)
	args := []string{"-p", port}
	if h.Jump != nil && h.Jump.Addr != "" {
		j := h.Jump.Addr
		if h.Jump.User != "" {
			j = h.Jump.User + "@" + j
		}
		args = append(args, "-J", j)
	}
	return append(args, dest)
}

// hasTag reports whether a host carries the given label.
func hasTag(h config.Host, want string) bool {
	for _, t := range h.Tags {
		if t == want {
			return true
		}
	}
	return false
}

// probeBastion checks a host that is reachable only through the jump server.
// The jump accepts an interactive session and nothing else, so ~/.tn/bastion.sh
// drives it with expect; $PPID makes the marker unpredictable, so the terminal
// echo of the typed command cannot satisfy the match.
func probeBastion(h config.Host, st poolState) poolState {
	ip := st.Addr
	if host, _, err := net.SplitHostPort(config.EnsureAddrPort(st.Addr)); err == nil {
		ip = host
	}
	ctx, cancel := context.WithTimeout(context.Background(), boardProbeTO)
	defer cancel()
	start := time.Now()
	out, err := exec.CommandContext(ctx, config.Home()+"/bastion.sh", ip, metricsCommand()).CombinedOutput()
	st.RTTMs = time.Since(start).Milliseconds()
	if err == nil {
		st.Up = true
		parseMetrics(string(out), &st)
		return st
	}
	st.Detail = firstLine(out, err)
	return st
}

func probePoolHost(h config.Host) poolState {
	st := poolState{Name: h.Name, Addr: h.Target.Addr, User: h.Target.User,
		CPU: -1, Mem: -1, Disk: -1, GPU: -1}
	if hasTag(h, "bastion-only") {
		return probeBastion(h, st)
	}
	args := append(sshArgs(h),
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=8",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "LogLevel=ERROR",
		metricsCommand())
	ctx, cancel := context.WithTimeout(context.Background(), boardProbeTO)
	defer cancel()
	start := time.Now()
	out, err := exec.CommandContext(ctx, "/usr/bin/ssh", args...).CombinedOutput()
	st.RTTMs = time.Since(start).Milliseconds()
	if err == nil {
		st.Up = true
		parseMetrics(string(out), &st)
		return st
	}
	st.Detail = firstLine(out, err)
	return st
}

func firstLine(out []byte, err error) string {
	msg := strings.TrimSpace(string(out))
	if msg == "" && err != nil {
		msg = err.Error()
	}
	if i := strings.IndexByte(msg, '\n'); i > 0 {
		msg = msg[:i]
	}
	return msg
}

// --- rendering ---------------------------------------------------------------

const (
	colReset = "\033[0m"
	colDim   = "\033[2m"
)

// colorFor maps a utilisation percentage to a TERM colour.
func colorFor(p int) string {
	switch {
	case p >= 85:
		return "31" // red
	case p >= 60:
		return "33" // yellow
	default:
		return "32" // green
	}
}

// cell renders one metric: optional bar, percentage, and the absolute figure.
// The absolute part is the point: "83%" alone does not say whether 83% is
// 8 GiB or 800 GiB.
// dot is the whole status column: green up, red down. A word ("up"/"down")
// costs four cells and says nothing a colour does not.
func dot(up bool, color bool) string {
	if !color {
		if up {
			return "o"
		}
		return "x"
	}
	if up {
		return "\033[32m●" + colReset
	}
	return "\033[31m●" + colReset
}

// cell renders one metric. Figures are right-aligned so decimal points line up
// down the column; colour marks the utilisation band.
func cell(p int, abs string, color bool) string {
	if p < 0 {
		return "               "
	}
	body := fmt.Sprintf("%3d%% %10s", p, abs)
	if color {
		return "\033[" + colorFor(p) + "m" + body + colReset
	}
	return body
}

// rttStr keeps the RTT column a fixed narrow width: milliseconds below one
// second, one decimal above it. "7173ms" wastes two cells and squeezes the
// metric columns on a 100-column terminal.
func rttStr(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	default:
		return fmt.Sprintf("%dm", ms/60000)
	}
}

// humanKB scales a KiB count to the largest unit that keeps it under 1000,
// with one decimal below 10. "512G" reads better than "536870912K", and the
// same cell has to hold a 4 TiB machine and a 20 GiB one.
func humanKB(v int64) string {
	f := float64(v) * 1024
	units := []string{"B", "K", "M", "G", "T", "P"}
	i := 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if f < 10 && i > 0 {
		return fmt.Sprintf("%.1f%s", f, units[i])
	}
	return fmt.Sprintf("%.0f%s", f, units[i])
}

// humanMB is humanKB for values already in MiB.
func humanMB(v int) string { return humanKB(int64(v) * 1024) }

func cpuAbs(s poolState) string {
	if s.NCPU <= 0 {
		return "--"
	}
	return fmt.Sprintf("%.2f/%d", s.Load, s.NCPU)
}

func memAbs(s poolState) string {
	if s.MemTotalKB <= 0 {
		return "--"
	}
	return humanKB(s.MemUsedKB) + "/" + humanKB(s.MemTotalKB)
}

func diskAbs(s poolState) string {
	if s.DiskTotalKB <= 0 {
		return "--"
	}
	return humanKB(s.DiskUsedKB) + "/" + humanKB(s.DiskTotalKB)
}

func gpuAbs(s poolState) string {
	if s.GPUTotalMB <= 0 {
		return "--"
	}
	return humanMB(s.GPUUsedMB) + "/" + humanMB(s.GPUTotalMB)
}

// renderPool formats the table. sel < 0 means no selection. width <= 0 means
// 120 columns; narrow terminals must pass their real width or the rows wrap
// and the redraw leaves debris. Bars are dropped below 96 columns; absolute
// figures are always kept.
func renderPool(states []poolState, sel int, tick int, width int, color bool) string {
	if width <= 0 {
		width = 120
	}
	var b strings.Builder
	up := 0
	for _, s := range states {
		if s.Up {
			up++
		}
	}
	if tick > 0 {
		fmt.Fprintf(&b, "tn board   %d/%d up   refresh %s   tick %d\n\n", up, len(states), boardInterval, tick)
	} else {
		fmt.Fprintf(&b, "tn pool   %d/%d up\n\n", up, len(states))
	}
	hdr := fmt.Sprintf("%-25s  %s %-6s %-15s %-15s %-15s %s",
		"HOST", " ", "RTT", "CPU", "MEM", "DISK", "GPU")
	if color {
		hdr = colDim + hdr + colReset
	}
	b.WriteString(hdr + "\n")

	for i, s := range states {
		state := dot(s.Up, color)
		rtt := "-"
		if s.Up {
			rtt = rttStr(s.RTTMs)
		}
		addr := s.Addr
		if host, _, err := net.SplitHostPort(config.EnsureAddrPort(s.Addr)); err == nil {
			addr = host
		}
		host := fmt.Sprintf("%-11s %s", s.Name, addr)
		var line string
		if s.Up {
			line = fmt.Sprintf("%-25s  %s %-6s %s %s %s %s", host, state, rtt,
				cell(s.CPU, cpuAbs(s), color),
				cell(s.Mem, memAbs(s), color),
				cell(s.Disk, diskAbs(s), color),
				cell(s.GPU, gpuAbs(s), color))
		} else {
			// No readings on a down host: show why instead of five blank columns.
			line = fmt.Sprintf("%-25s  %s %-6s %s", host, state, rtt, s.Detail)
		}
		line = truncate(line, width)
		if i == sel && color {
			line = "\033[7m" + line + colReset
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteString("\nj/k move   enter connect   r refresh   q quit\n")
	return b.String()
}

// truncate cuts to n visible cells, keeping ANSI escapes intact and counting
// none of their bytes.
func truncate(s string, n int) string {
	var out strings.Builder
	vis := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			if j < len(s) {
				j++
			}
			out.WriteString(s[i:j])
			i = j
			continue
		}
		_, size := utf8DecodeRune(s[i:])
		if vis >= n-1 {
			out.WriteString("…")
			out.WriteString(colReset)
			return out.String()
		}
		out.WriteString(s[i : i+size])
		vis++
		i += size
	}
	return out.String()
}

// utf8DecodeRune avoids importing unicode/utf8 just for the width loop.
func utf8DecodeRune(s string) (rune, int) {
	b := s[0]
	switch {
	case b < 0x80:
		return rune(b), 1
	case b>>5 == 0x6:
		return 0, 2
	case b>>4 == 0xe:
		return 0, 3
	default:
		return 0, 4
	}
}
