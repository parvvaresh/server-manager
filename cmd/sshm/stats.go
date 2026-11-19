package main

// Server health for the dashboard: one round trip reads /proc twice, a second
// apart, so CPU and network can be reported as rates rather than counters.

import (
	"bufio"
	"bytes"
	"context"
	"strconv"
	"strings"
	"time"
)

const statsScript = `export LC_ALL=C
echo @@host; hostname 2>/dev/null || uname -n
echo @@os; if [ -r /etc/os-release ]; then . /etc/os-release; echo "$PRETTY_NAME"; else uname -s; fi
echo @@kernel; uname -srm
echo @@cores; nproc 2>/dev/null || grep -c ^processor /proc/cpuinfo 2>/dev/null
echo @@cpumodel; grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2-
echo @@up1; cat /proc/uptime 2>/dev/null
echo @@stat1; head -n1 /proc/stat 2>/dev/null
echo @@net1; cat /proc/net/dev 2>/dev/null
sleep 1
echo @@up2; cat /proc/uptime 2>/dev/null
echo @@stat2; head -n1 /proc/stat 2>/dev/null
echo @@net2; cat /proc/net/dev 2>/dev/null
echo @@load; cat /proc/loadavg 2>/dev/null
echo @@mem; cat /proc/meminfo 2>/dev/null
echo @@disk; df -P -k 2>/dev/null
echo @@users; who 2>/dev/null | wc -l
echo @@procs; ps -eo pid,pcpu,pmem,rss,comm --sort=-pcpu 2>/dev/null | head -n 9
exit 0
`

type Disk struct {
	FS    string `json:"fs"`
	Mount string `json:"mount"`
	Size  uint64 `json:"size"`
	Used  uint64 `json:"used"`
}

type Proc struct {
	PID int     `json:"pid"`
	CPU float64 `json:"cpu"`
	Mem float64 `json:"mem"`
	RSS uint64  `json:"rss"`
	Cmd string  `json:"cmd"`
}

type Stats struct {
	Host      string     `json:"host"`
	OS        string     `json:"os"`
	Kernel    string     `json:"kernel"`
	CPUModel  string     `json:"cpuModel"`
	Cores     int        `json:"cores"`
	Uptime    float64    `json:"uptime"`
	Load      [3]float64 `json:"load"`
	CPU       float64    `json:"cpu"` // percent busy, -1 when unknown
	MemTotal  uint64     `json:"memTotal"`
	MemUsed   uint64     `json:"memUsed"`
	MemCache  uint64     `json:"memCache"`
	SwapTotal uint64     `json:"swapTotal"`
	SwapUsed  uint64     `json:"swapUsed"`
	NetRx     float64    `json:"netRx"` // bytes per second
	NetTx     float64    `json:"netTx"`
	Users     int        `json:"users"`
	Disks     []Disk     `json:"disks"`
	Procs     []Proc     `json:"procs"`
	Latency   int64      `json:"latency"` // ms for the whole round trip
}

func collectStats(ctx context.Context, s Server) (*Stats, error) {
	start := time.Now()
	out, err := remoteOutput(ctx, s, statsScript)
	if err != nil {
		return nil, err
	}
	st := parseStats(out)
	st.Latency = time.Since(start).Milliseconds() - 1000 // minus the sleep
	if st.Latency < 0 {
		st.Latency = 0
	}
	return st, nil
}

func splitSections(out []byte) map[string][]string {
	sec := map[string][]string{}
	cur := ""
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		ln := sc.Text()
		if strings.HasPrefix(ln, "@@") {
			cur = strings.TrimSpace(ln[2:])
			sec[cur] = sec[cur][:0]
			continue
		}
		if cur != "" {
			sec[cur] = append(sec[cur], ln)
		}
	}
	return sec
}

func first(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.TrimSpace(lines[0])
}

func parseStats(out []byte) *Stats {
	sec := splitSections(out)
	st := &Stats{CPU: -1, Disks: []Disk{}, Procs: []Proc{}}
	st.Host = first(sec["host"])
	st.OS = first(sec["os"])
	st.Kernel = first(sec["kernel"])
	st.CPUModel = strings.Join(strings.Fields(first(sec["cpumodel"])), " ")
	st.Cores, _ = strconv.Atoi(first(sec["cores"]))
	st.Users, _ = strconv.Atoi(first(sec["users"]))

	up1, up2 := uptimeOf(sec["up1"]), uptimeOf(sec["up2"])
	st.Uptime = up2
	interval := up2 - up1
	if interval <= 0 {
		interval = 1
	}

	if f := strings.Fields(first(sec["load"])); len(f) >= 3 {
		for i := 0; i < 3; i++ {
			st.Load[i], _ = strconv.ParseFloat(f[i], 64)
		}
	}

	busy1, total1 := cpuTimes(first(sec["stat1"]))
	busy2, total2 := cpuTimes(first(sec["stat2"]))
	if dt := total2 - total1; dt > 0 && total1 > 0 {
		st.CPU = clampF(100*float64(busy2-busy1)/float64(dt), 0, 100)
	}

	rx1, tx1 := netBytes(sec["net1"])
	rx2, tx2 := netBytes(sec["net2"])
	if rx2 >= rx1 && tx2 >= tx1 && (rx1 > 0 || tx1 > 0) {
		st.NetRx = float64(rx2-rx1) / interval
		st.NetTx = float64(tx2-tx1) / interval
	}

	mem := map[string]uint64{}
	for _, ln := range sec["mem"] {
		k, v, ok := strings.Cut(ln, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		n, _ := strconv.ParseUint(f[0], 10, 64)
		mem[k] = n * 1024
	}
	st.MemTotal = mem["MemTotal"]
	avail, ok := mem["MemAvailable"]
	if !ok {
		avail = mem["MemFree"] + mem["Buffers"] + mem["Cached"]
	}
	if st.MemTotal >= avail {
		st.MemUsed = st.MemTotal - avail
	}
	st.MemCache = mem["Buffers"] + mem["Cached"]
	st.SwapTotal = mem["SwapTotal"]
	if st.SwapTotal >= mem["SwapFree"] {
		st.SwapUsed = st.SwapTotal - mem["SwapFree"]
	}

	st.Disks = parseDisks(sec["disk"])
	st.Procs = parseProcs(sec["procs"])
	return st
}

func uptimeOf(lines []string) float64 {
	f := strings.Fields(first(lines))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return v
}

// cpuTimes reads the aggregate "cpu" line of /proc/stat. Idle and iowait
// count as idle; guest time is already included in user time.
func cpuTimes(line string) (busy, total uint64) {
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0
	}
	for i, v := range f[1:] {
		if i >= 8 {
			break
		}
		n, _ := strconv.ParseUint(v, 10, 64)
		total += n
		if i != 3 && i != 4 {
			busy += n
		}
	}
	return busy, total
}

// netBytes sums received and sent bytes over every interface but loopback.
func netBytes(lines []string) (rx, tx uint64) {
	for _, ln := range lines {
		name, rest, ok := strings.Cut(ln, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "lo" || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "docker") ||
			strings.HasPrefix(name, "br-") {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		r, _ := strconv.ParseUint(f[0], 10, 64)
		t, _ := strconv.ParseUint(f[8], 10, 64)
		rx += r
		tx += t
	}
	return rx, tx
}

var pseudoFS = map[string]bool{
	"tmpfs": true, "devtmpfs": true, "udev": true, "overlay": true, "shm": true,
	"none": true, "squashfs": true, "efivarfs": true, "cgroup": true, "cgroup2": true,
}

func parseDisks(lines []string) []Disk {
	out := []Disk{}
	seen := map[string]bool{}
	for i, ln := range lines {
		if i == 0 {
			continue // header
		}
		f := strings.Fields(ln)
		if len(f) < 6 {
			continue
		}
		fs, mount := f[0], strings.Join(f[5:], " ")
		if pseudoFS[fs] || seen[fs] ||
			strings.HasPrefix(mount, "/snap/") || strings.HasPrefix(mount, "/run") ||
			strings.HasPrefix(mount, "/dev") || strings.HasPrefix(mount, "/sys") ||
			strings.HasPrefix(mount, "/proc") || strings.HasPrefix(mount, "/boot/efi") ||
			strings.HasPrefix(mount, "/System/Volumes/") {
			continue
		}
		size, _ := strconv.ParseUint(f[1], 10, 64)
		used, _ := strconv.ParseUint(f[2], 10, 64)
		if size == 0 {
			continue
		}
		seen[fs] = true
		out = append(out, Disk{FS: fs, Mount: mount, Size: size * 1024, Used: used * 1024})
	}
	return out
}

func parseProcs(lines []string) []Proc {
	out := []Proc{}
	for i, ln := range lines {
		if i == 0 {
			continue // header
		}
		f := strings.Fields(ln)
		if len(f) < 5 {
			continue
		}
		var p Proc
		p.PID, _ = strconv.Atoi(f[0])
		p.CPU, _ = strconv.ParseFloat(f[1], 64)
		p.Mem, _ = strconv.ParseFloat(f[2], 64)
		rss, _ := strconv.ParseUint(f[3], 10, 64)
		p.RSS = rss * 1024
		p.Cmd = strings.Join(f[4:], " ")
		out = append(out, p)
	}
	return out
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
