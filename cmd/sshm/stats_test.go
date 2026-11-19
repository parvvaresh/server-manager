package main

import "testing"

// linuxSample is what statsScript prints on a typical Linux box.
const linuxSample = `@@host
web-1
@@os
Ubuntu 24.04.1 LTS
@@kernel
Linux 6.8.0-45-generic x86_64
@@cores
4
@@cpumodel
 Intel(R) Xeon(R)   CPU E5-2680 v4 @ 2.40GHz
@@up1
1000.00 3900.00
@@stat1
cpu  1000 0 1000 7000 1000 0 0 0 0 0
@@net1
Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:  500000    100    0    0    0     0          0         0   500000     100    0    0    0     0       0          0
  eth0: 1000000   1000    0    0    0     0          0         0  2000000    1000    0    0    0     0       0          0
@@up2
1002.00 3907.00
@@stat2
cpu  1300 0 1300 7300 1100 0 0 0 0 0
@@net2
Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:  900000    200    0    0    0     0          0         0   900000     200    0    0    0     0       0          0
  eth0: 1200000   1100    0    0    0     0          0         0  2100000    1100    0    0    0     0       0          0
@@load
0.52 0.40 0.31 1/234 5678
@@mem
MemTotal:        8000000 kB
MemFree:         1000000 kB
MemAvailable:    6000000 kB
Buffers:          200000 kB
Cached:          3000000 kB
SwapTotal:       2000000 kB
SwapFree:        1500000 kB
@@disk
Filesystem     1024-blocks     Used Available Capacity Mounted on
/dev/sda1         41000000 20500000  20500000      50% /
tmpfs              4000000        0   4000000       0% /dev/shm
/dev/sdb1        100000000 10000000  90000000      10% /mnt/my data
/dev/loop0           65536    65536         0     100% /snap/core/123
@@users
2
@@procs
    PID %CPU %MEM   RSS COMMAND
   1234 12.5  3.2 262144 postgres
      1  0.0  0.1  12000 systemd
`

func TestParseStatsLinux(t *testing.T) {
	st := parseStats([]byte(linuxSample))

	if st.Host != "web-1" || st.OS != "Ubuntu 24.04.1 LTS" || st.Cores != 4 || st.Users != 2 {
		t.Fatalf("identity: %+v", st)
	}
	if st.CPUModel != "Intel(R) Xeon(R) CPU E5-2680 v4 @ 2.40GHz" {
		t.Errorf("cpu model %q", st.CPUModel)
	}
	// busy went 2000 -> 2600 out of a total 10000 -> 11000: 60%
	if st.CPU < 59.9 || st.CPU > 60.1 {
		t.Errorf("cpu = %v, want 60", st.CPU)
	}
	if st.Uptime != 1002 || st.Load != [3]float64{0.52, 0.40, 0.31} {
		t.Errorf("uptime %v load %v", st.Uptime, st.Load)
	}
	// loopback is ignored; eth0 moved 200000 / 100000 bytes over 2 seconds
	if st.NetRx != 100000 || st.NetTx != 50000 {
		t.Errorf("net rx %v tx %v", st.NetRx, st.NetTx)
	}
	if st.MemTotal != 8000000*1024 || st.MemUsed != 2000000*1024 || st.MemCache != 3200000*1024 {
		t.Errorf("mem %d used %d cache %d", st.MemTotal, st.MemUsed, st.MemCache)
	}
	if st.SwapTotal != 2000000*1024 || st.SwapUsed != 500000*1024 {
		t.Errorf("swap %d used %d", st.SwapTotal, st.SwapUsed)
	}
	if len(st.Disks) != 2 || st.Disks[0].Mount != "/" || st.Disks[1].Mount != "/mnt/my data" {
		t.Fatalf("disks %+v", st.Disks)
	}
	if st.Disks[0].Used != 20500000*1024 {
		t.Errorf("disk used %d", st.Disks[0].Used)
	}
	if len(st.Procs) != 2 || st.Procs[0].Cmd != "postgres" || st.Procs[0].CPU != 12.5 || st.Procs[0].RSS != 262144*1024 {
		t.Errorf("procs %+v", st.Procs)
	}
}

func TestParseStatsEmpty(t *testing.T) {
	st := parseStats(nil)
	if st.CPU != -1 || st.Disks == nil || st.Procs == nil {
		t.Errorf("empty output should give unknown CPU and empty lists: %+v", st)
	}
}

func TestParseEntries(t *testing.T) {
	e, ok := parseFindEntry("ld\t7\t1700000000.5\t777\tcurrent")
	if !ok || e.Type != "link" || !e.IsDir || e.Name != "current" || e.MTime != 1700000000.5 {
		t.Errorf("find link: %+v", e)
	}
	e, ok = parseFindEntry("ff\t1024\t1700000000\t644\tname\twith\ttabs")
	if !ok || e.Type != "file" || e.IsDir || e.Name != "name\twith\ttabs" || e.Size != 1024 {
		t.Errorf("find file: %+v", e)
	}
	e, ok = parseStatEntry("d|Directory|64|1700000000|755|my|dir")
	if !ok || e.Type != "dir" || !e.IsDir || e.Name != "my|dir" {
		t.Errorf("bsd stat dir: %+v", e)
	}
	e, ok = parseStatEntry("f|regular empty file|0|1700000000|600|empty")
	if !ok || e.Type != "file" || e.IsDir {
		t.Errorf("gnu stat file: %+v", e)
	}
}

func TestPathValidation(t *testing.T) {
	for _, bad := range []string{"", "relative/path", "/a\x00b"} {
		if _, err := remotePath(bad); err == nil {
			t.Errorf("remotePath(%q) should fail", bad)
		}
	}
	if p, _ := remotePath("/var/www/../log/"); p != "/var/log" {
		t.Errorf("remotePath cleaned to %q", p)
	}
	for _, bad := range []string{"", ".", "..", "a/b", " "} {
		if _, err := fileName(bad); err == nil {
			t.Errorf("fileName(%q) should fail", bad)
		}
	}
}

func TestRemoteScriptQuoting(t *testing.T) {
	got := remoteScript(`echo "$1"`, "it's; rm -rf /")
	want := `sh -c 'echo "$1"' sshm 'it'\''s; rm -rf /'`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}
