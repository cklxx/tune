package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cklxx/tune/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var boardCmd = &cobra.Command{
	Use:   "board",
	Short: "Live dashboard over the host pool",
	Long: `Renders a refreshing table of every host in ~/.tn/config.yaml: state, RTT,
CPU, memory, disk and GPU utilisation, colour-coded green/yellow/red.

Keys: j/k or arrows move, enter connects with "ssh -t", r refreshes now,
q quits. Kerberos is refreshed through ~/.tn/kt.sh at start and on every
refresh, so GSSAPI-only hosts stay reachable without a manual kinit.

Jump-server hosts are polled on a slower channel: each probe drives a real
interactive session and takes up to 20s, which would stall the table.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runBoard()
	},
}

func init() {
	boardCmd.Flags().DurationVar(&boardInterval, "interval", 5*time.Second, "refresh interval")
}

func runBoard() error {
	hosts := loadPool(false)
	if len(hosts) == 0 {
		return fmt.Errorf("no hosts with an address in %s/config.yaml", config.Home())
	}
	_ = exec.Command("sh", config.Home()+"/kt.sh").Run() // best-effort ticket

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return fmt.Errorf("board needs a terminal (use 'tn pool' for a one-shot table)")
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	defer term.Restore(fd, old)
	fmt.Print("\033[?25l")
	defer fmt.Print("\033[?25h")

	var (
		mu     sync.Mutex // guards states, tick and sel
		drawMu sync.Mutex // stdout is shared by the input loop and the refreshers
		tick   int
		sel    int
	)

	states := make([]poolState, len(hosts))
	index := make(map[string]int, len(hosts))
	for i, h := range hosts {
		index[h.Name] = i
		states[i] = poolState{
			Name: h.Name, Addr: h.Target.Addr, User: h.Target.User,
			CPU: -1, Mem: -1, Disk: -1, GPU: -1, Detail: "probing…",
		}
	}

	draw := func() {
		mu.Lock()
		s := append([]poolState(nil), states...)
		tk, sl := tick, sel
		mu.Unlock()
		if len(s) == 0 {
			return
		}
		w := 120
		if cols, _, err := term.GetSize(fd); err == nil && cols > 0 {
			w = cols
		}
		drawMu.Lock()
		// Raw mode disables ONLCR, so a bare \n moves down without returning the
		// carriage: every line would start where the previous one ended.
		fmt.Print("\033[H\033[2J" + strings.ReplaceAll(renderPool(s, sl, tk, w, true), "\n", "\r\n"))
		drawMu.Unlock()
	}

	// Probe results arrive from many goroutines. Redrawing once per result made
	// the terminal the bottleneck and the UI felt stuck while the network was
	// slow. The probes only mark the view dirty; one goroutine paints.
	var dirty atomic.Bool
	requestDraw := func() { dirty.Store(true) }

	lastProbe := make(map[string]time.Time, len(hosts))

	merge := func(group []config.Host, st []poolState) {
		mu.Lock()
		for i, h := range group {
			if i < len(st) {
				if idx, ok := index[h.Name]; ok {
					states[idx] = st[i]
				}
			}
		}
		tick++
		mu.Unlock()
		requestDraw()
	}

	// Same as merge, but for results that arrive one at a time.
	mergeOne := func(group []config.Host) func(int, poolState) {
		return func(i int, st poolState) {
			if i >= len(group) {
				return
			}
			mu.Lock()
			if idx, ok := index[group[i].Name]; ok {
				states[idx] = st
				lastProbe[group[i].Name] = time.Now()
			}
			tick++
			mu.Unlock()
			requestDraw()
		}
	}

	// Split by probe cost. Jump-server hosts take up to 20s each; they must not
	// sit inside the fast loop or the table freezes at one frame per 20s.
	var fast, slow []config.Host
	for _, h := range hosts {
		if hasTag(h, "bastion-only") {
			slow = append(slow, h)
		} else {
			fast = append(fast, h)
		}
	}

	// One round per group at a time. A slow host (a jump hop costs 20s) would
	// otherwise stack rounds: at a 5s interval the board reached 20 concurrent
	// ssh processes and the whole machine crawled.
	// Cancelling this context on exit kills in-flight probes, including the
	// children of a bastion hop, so quitting leaves no ssh behind.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer killProbes()

	// due returns the hosts whose next probe is worth paying for. A session on
	// these machines costs 4-10s regardless of the command, so probing a host
	// that just answered 2s ago only adds load.
	due := func(group []config.Host) []config.Host {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		out := make([]config.Host, 0, len(group))
		for _, h := range group {
			gap := boardInterval
			if idx, ok := index[h.Name]; ok {
				if st := states[idx]; st.Up && st.RTTMs > 0 {
					if d := time.Duration(st.RTTMs*2) * time.Millisecond; d > gap {
						gap = d
					}
				}
			}
			if last, ok := lastProbe[h.Name]; !ok || now.Sub(last) >= gap {
				out = append(out, h)
			}
		}
		return out
	}

	var fastBusy, slowBusy atomic.Bool
	refreshFast := func() {
		if !fastBusy.CompareAndSwap(false, true) {
			return
		}
		go func() {
			defer fastBusy.Store(false)
			group := due(fast)
			if len(group) == 0 {
				return
			}
			probeStream(ctx, group, mergeOne(group))
		}()
	}
	refreshSlow := func() {
		if !slowBusy.CompareAndSwap(false, true) {
			return
		}
		go func() {
			defer slowBusy.Store(false)
			merge(slow, probePool(ctx, slow))
		}()
	}

	draw() // first frame before any probe, so a slow ssh does not look like a hang
	if len(fast) > 0 {
		refreshFast()
	}

	stop := make(chan struct{})
	defer close(stop)

	go func() {
		t := time.NewTicker(120 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if dirty.CompareAndSwap(true, false) {
					draw()
				}
			}
		}
	}()

	if len(slow) > 0 {
		go func() {
			merge(slow, probePool(ctx, slow))
			t := time.NewTicker(60 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					refreshSlow()
				}
			}
		}()
	}

	go func() {
		t := time.NewTicker(boardInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				_ = exec.Command("sh", config.Home()+"/kt.sh").Run()
				merge(fast, probePool(ctx, fast))
			}
		}
	}()

	buf := make([]byte, 3)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return nil
		}
		switch {
		case n == 1 && (buf[0] == 'q' || buf[0] == 3):
			return nil
		case n == 1 && (buf[0] == 'j' || buf[0] == 14):
			mu.Lock()
			if sel < len(hosts)-1 {
				sel++
			}
			mu.Unlock()
		case n == 1 && (buf[0] == 'k' || buf[0] == 16):
			mu.Lock()
			if sel > 0 {
				sel--
			}
			mu.Unlock()
		case n == 3 && buf[0] == 27 && buf[1] == '[':
			mu.Lock()
			if buf[2] == 'B' && sel < len(hosts)-1 {
				sel++
			}
			if buf[2] == 'A' && sel > 0 {
				sel--
			}
			mu.Unlock()
		case n == 1 && buf[0] == 'r':
			refreshFast()
			if len(slow) > 0 {
				refreshSlow()
			}
		case n == 1 && (buf[0] == '\r' || buf[0] == '\n'):
			mu.Lock()
			h := hosts[sel]
			mu.Unlock()
			fmt.Print("\033[?25h")
			term.Restore(fd, old)
			var c *exec.Cmd
			if hasTag(h, "bastion-only") {
				ip := h.Target.Addr
				if host, _, err := net.SplitHostPort(config.EnsureAddrPort(ip)); err == nil {
					ip = host
				}
				c = exec.Command(config.Home()+"/bastion.sh", ip)
			} else {
				c = exec.Command("/usr/bin/ssh", append([]string{"-t"}, sshArgs(h)...)...)
			}
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			_ = c.Run()
			if _, err := term.MakeRaw(fd); err != nil {
				return err
			}
			fmt.Print("\033[?25l")
			refreshFast()
		}
		draw()
	}
}
