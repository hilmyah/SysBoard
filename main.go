package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	listenPort  string
	staticToken string
	logFilePath string
)

// ─── Middleware ───────────────────────────────────────────────────────────────

func jsonHeader(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "X-Auth-Token, Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonHeader(w)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Header.Get("X-Auth-Token") != staticToken {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// ─── System Metrics ───────────────────────────────────────────────────────────

type SystemMetrics struct {
	CPUPercent float64    `json:"cpu_percent"`
	RAMTotal   uint64     `json:"ram_total"`
	RAMUsed    uint64     `json:"ram_used"`
	RAMPercent float64    `json:"ram_percent"`
	Disks      []DiskInfo `json:"disks"`
	Uptime     string     `json:"uptime"`
	LoadAvg    string     `json:"load_avg"`
	Timestamp  string     `json:"timestamp"`
	NetRxBytes uint64     `json:"net_rx_bytes"`
	NetTxBytes uint64     `json:"net_tx_bytes"`
	NetRxRate  float64    `json:"net_rx_rate"`
	NetTxRate  float64    `json:"net_tx_rate"`
	CPUTemp    float64    `json:"cpu_temp"`
}

type DiskInfo struct {
	Mount   string  `json:"mount"`
	Total   uint64  `json:"total"`
	Used    uint64  `json:"used"`
	Free    uint64  `json:"free"`
	Percent float64 `json:"percent"`
}

func parseProcStat() (idle, total uint64) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "cpu ") {
			fields := strings.Fields(line)
			var vals []uint64
			for _, s := range fields[1:] {
				v, _ := strconv.ParseUint(s, 10, 64)
				vals = append(vals, v)
			}
			if len(vals) >= 4 {
				idle = vals[3]
				for _, v := range vals {
					total += v
				}
			}
			break
		}
	}
	return
}

func getCPUPercent() float64 {
	idle1, total1 := parseProcStat()
	time.Sleep(200 * time.Millisecond)
	idle2, total2 := parseProcStat()
	deltaTotal := total2 - total1
	deltaIdle := idle2 - idle1
	if deltaTotal == 0 {
		return 0
	}
	return (1 - float64(deltaIdle)/float64(deltaTotal)) * 100
}

func getMemInfo() (total, used, free uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return
	}
	defer f.Close()
	vals := map[string]uint64{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 {
			v, _ := strconv.ParseUint(fields[1], 10, 64)
			vals[strings.TrimSuffix(fields[0], ":")] = v * 1024
		}
	}
	total = vals["MemTotal"]
	free = vals["MemAvailable"]
	used = total - free
	return
}

// getDiskMounts reads /proc/mounts and returns real block-device mount points,
// excluding tmpfs, devtmpfs, sysfs, proc, cgroup, and overlay.
func getDiskMounts() []string {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return []string{"/"}
	}
	defer f.Close()

	skip := map[string]bool{
		"tmpfs": true, "devtmpfs": true, "sysfs": true, "proc": true,
		"cgroup": true, "cgroup2": true, "pstore": true, "securityfs": true,
		"debugfs": true, "configfs": true, "fusectl": true, "hugetlbfs": true,
		"mqueue": true, "bpf": true, "tracefs": true, "efivarfs": true,
		"autofs": true, "overlay": true, "squashfs": true,
	}
	skipPrefix := []string{"/sys", "/proc", "/dev", "/run"}

	seen := map[string]bool{}
	var mounts []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		fstype := fields[2]
		mount := fields[1]
		if skip[fstype] {
			continue
		}
		ignored := false
		for _, p := range skipPrefix {
			if strings.HasPrefix(mount, p) {
				ignored = true
				break
			}
		}
		if ignored || seen[mount] {
			continue
		}
		seen[mount] = true
		mounts = append(mounts, mount)
	}
	if len(mounts) == 0 {
		return []string{"/"}
	}
	sort.Strings(mounts)
	return mounts
}

func getDiskInfo(mount string) DiskInfo {
	out, err := exec.Command("df", "-B1", mount).Output()
	if err != nil {
		return DiskInfo{Mount: mount}
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) >= 2 {
		fields := strings.Fields(lines[1])
		if len(fields) >= 5 {
			total, _ := strconv.ParseUint(fields[1], 10, 64)
			used, _ := strconv.ParseUint(fields[2], 10, 64)
			free, _ := strconv.ParseUint(fields[3], 10, 64)
			pct := 0.0
			if total > 0 {
				pct = float64(used) / float64(total) * 100
			}
			return DiskInfo{Mount: mount, Total: total, Used: used, Free: free, Percent: pct}
		}
	}
	return DiskInfo{Mount: mount}
}

type netStat struct {
	rx, tx uint64
}

func readNetStats() netStat {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return netStat{}
	}
	defer f.Close()
	var rx, tx uint64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "lo:") || !strings.Contains(line, ":") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) >= 9 {
			r, _ := strconv.ParseUint(fields[0], 10, 64)
			t, _ := strconv.ParseUint(fields[8], 10, 64)
			rx += r
			tx += t
		}
	}
	return netStat{rx: rx, tx: tx}
}

var lastNetStat netStat
var lastNetTime time.Time

func getNetRate() (rxBytes, txBytes uint64, rxRate, txRate float64) {
	cur := readNetStats()
	now := time.Now()
	if !lastNetTime.IsZero() {
		elapsed := now.Sub(lastNetTime).Seconds()
		if elapsed > 0 {
			rxRate = float64(cur.rx-lastNetStat.rx) / elapsed
			txRate = float64(cur.tx-lastNetStat.tx) / elapsed
		}
	}
	lastNetStat = cur
	lastNetTime = now
	return cur.rx, cur.tx, rxRate, txRate
}

func getCPUTemp() float64 {
	matches, err := filepath.Glob("/sys/class/hwmon/hwmon*/temp*_input")
	if err == nil {
		for _, m := range matches {
			data, err := os.ReadFile(m)
			if err == nil {
				v, _ := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
				if v > 0 {
					return v / 1000.0
				}
			}
		}
	}
	for i := 0; i < 5; i++ {
		data, err := os.ReadFile(fmt.Sprintf("/sys/class/thermal/thermal_zone%d/temp", i))
		if err == nil {
			v, _ := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
			if v > 1000 {
				return v / 1000.0
			}
		}
	}
	return 0
}

func handleMetrics(w http.ResponseWriter, r *http.Request) {
	cpu := getCPUPercent()
	total, used, _ := getMemInfo()
	ramPct := 0.0
	if total > 0 {
		ramPct = float64(used) / float64(total) * 100
	}

	mounts := getDiskMounts()
	disks := make([]DiskInfo, 0, len(mounts))
	for _, m := range mounts {
		disks = append(disks, getDiskInfo(m))
	}

	uptimeOut, _ := os.ReadFile("/proc/uptime")
	uptime := "unknown"
	if len(uptimeOut) > 0 {
		var secs float64
		fmt.Sscanf(string(uptimeOut), "%f", &secs)
		d := time.Duration(secs) * time.Second
		uptime = fmt.Sprintf("%dd %dh %dm", int(d.Hours())/24, int(d.Hours())%24, int(d.Minutes())%60)
	}
	loadOut, _ := os.ReadFile("/proc/loadavg")
	loadAvg := "unknown"
	if len(loadOut) > 0 {
		fields := strings.Fields(string(loadOut))
		if len(fields) >= 3 {
			loadAvg = strings.Join(fields[:3], " ")
		}
	}

	rxBytes, txBytes, rxRate, txRate := getNetRate()
	cpuTemp := getCPUTemp()

	json.NewEncoder(w).Encode(SystemMetrics{
		CPUPercent: cpu, RAMTotal: total, RAMUsed: used, RAMPercent: ramPct,
		Disks: disks, Uptime: uptime, LoadAvg: loadAvg, Timestamp: time.Now().Format("15:04:05"),
		NetRxBytes: rxBytes, NetTxBytes: txBytes, NetRxRate: rxRate, NetTxRate: txRate,
		CPUTemp: cpuTemp,
	})
}

// ─── Top Processes ────────────────────────────────────────────────────────────

type ProcessInfo struct {
	PID     int     `json:"pid"`
	Name    string  `json:"name"`
	CPUPct  float64 `json:"cpu_pct"`
	MemBytes uint64 `json:"mem_bytes"`
	State   string  `json:"state"`
	User    string  `json:"user"`
}

// readProcStat reads /proc/<pid>/stat and returns utime+stime in ticks, plus state and comm.
func readProcStat(pid int) (name, state string, ticks uint64) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return
	}
	s := string(data)
	// comm is between first '(' and last ')' to handle names with spaces/parens
	start := strings.Index(s, "(")
	end := strings.LastIndex(s, ")")
	if start < 0 || end < 0 || end <= start {
		return
	}
	name = s[start+1 : end]
	rest := strings.Fields(s[end+2:])
	if len(rest) < 12 {
		return
	}
	state = rest[0]
	utime, _ := strconv.ParseUint(rest[11], 10, 64)
	stime, _ := strconv.ParseUint(rest[12], 10, 64)
	ticks = utime + stime
	return
}

func readProcMem(pid int) uint64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				v, _ := strconv.ParseUint(fields[1], 10, 64)
				return v * 1024
			}
		}
	}
	return 0
}

func readProcUser(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return ""
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Uid:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				uid := fields[1]
				// Resolve common UIDs without calling id/getent
				switch uid {
				case "0":
					return "root"
				default:
					// Try /etc/passwd fast scan
					return resolveUID(uid)
				}
			}
		}
	}
	return ""
}

func resolveUID(uid string) string {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return uid
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), ":", 4)
		if len(parts) >= 3 && parts[2] == uid {
			return parts[0]
		}
	}
	return uid
}

// clkTck is the number of clock ticks per second (typically 100 on Linux).
const clkTck = 100

var (
	prevProcTicks = map[int]uint64{}
	prevProcTime  time.Time
)

func handleProcesses(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		json.NewEncoder(w).Encode([]ProcessInfo{})
		return
	}

	now := time.Now()
	elapsed := now.Sub(prevProcTime).Seconds()
	if prevProcTime.IsZero() {
		elapsed = 1
	}
	prevProcTime = now

	curTicks := map[int]uint64{}
	var procs []ProcessInfo

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		name, state, ticks := readProcStat(pid)
		if name == "" {
			continue
		}
		curTicks[pid] = ticks
		cpuPct := 0.0
		if prev, ok := prevProcTicks[pid]; ok && elapsed > 0 {
			delta := float64(ticks-prev) / clkTck
			cpuPct = (delta / elapsed) * 100
		}
		mem := readProcMem(pid)
		user := readProcUser(pid)
		procs = append(procs, ProcessInfo{
			PID: pid, Name: name, CPUPct: cpuPct,
			MemBytes: mem, State: state, User: user,
		})
	}

	prevProcTicks = curTicks

	// Sort by CPU desc, then by MEM desc as tiebreaker
	sort.Slice(procs, func(i, j int) bool {
		if procs[i].CPUPct != procs[j].CPUPct {
			return procs[i].CPUPct > procs[j].CPUPct
		}
		return procs[i].MemBytes > procs[j].MemBytes
	})

	top := 30
	if len(procs) < top {
		top = len(procs)
	}
	json.NewEncoder(w).Encode(procs[:top])
}

// ─── Systemd Services ─────────────────────────────────────────────────────────

type ServiceStatus struct {
	Name      string `json:"name"`
	Active    string `json:"active"`
	Sub       string `json:"sub"`
	Desc      string `json:"desc"`
	MemBytes  uint64 `json:"mem_bytes"`
	UptimeSec int64  `json:"uptime_sec"`
	UptimeStr string `json:"uptime_str"`
}

func getServiceMemAndUptime(name string) (memBytes uint64, uptimeSec int64) {
	out, err := exec.Command("systemctl", "show", name+".service",
		"--property=MemoryCurrent,ActiveEnterTimestamp").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "MemoryCurrent=") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "MemoryCurrent="))
			if val != "[not set]" && val != "" {
				v, err := strconv.ParseUint(val, 10, 64)
				if err == nil {
					memBytes = v
				}
			}
		}
		if strings.HasPrefix(line, "ActiveEnterTimestamp=") {
			ts := strings.TrimSpace(strings.TrimPrefix(line, "ActiveEnterTimestamp="))
			if ts != "" && ts != "n/a" {
				t, err := time.Parse("Mon 2006-01-02 15:04:05 MST", ts)
				if err == nil {
					uptimeSec = int64(time.Since(t).Seconds())
				}
			}
		}
	}
	return
}

func uptimeFmt(sec int64) string {
	if sec <= 0 {
		return ""
	}
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	if sec < 3600 {
		return fmt.Sprintf("%dm%ds", sec/60, sec%60)
	}
	if sec < 86400 {
		return fmt.Sprintf("%dh%dm", sec/3600, (sec%3600)/60)
	}
	return fmt.Sprintf("%dd%dh", sec/86400, (sec%86400)/3600)
}

func handleServices(w http.ResponseWriter, r *http.Request) {
	out, err := exec.Command("systemctl", "list-units", "--type=service",
		"--all", "--no-pager", "--plain", "--no-legend").Output()
	var statuses []ServiceStatus
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 4 {
				continue
			}
			name := strings.TrimSuffix(fields[0], ".service")
			active := fields[2]
			sub := fields[3]
			desc := strings.Join(fields[4:], " ")
			var mem uint64
			var upSec int64
			if active == "active" {
				mem, upSec = getServiceMemAndUptime(name)
			}
			statuses = append(statuses, ServiceStatus{
				Name: name, Active: active, Sub: sub, Desc: desc,
				MemBytes: mem, UptimeSec: upSec, UptimeStr: uptimeFmt(upSec),
			})
		}
	}
	json.NewEncoder(w).Encode(statuses)
}

func handleServiceAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	var req map[string]string
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	action, svc := req["action"], req["service"]
	allowed := map[string]bool{"start": true, "stop": true, "restart": true}
	if !allowed[action] || svc == "" || strings.ContainsAny(svc, " ;|&`$(){}\\") {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}
	out, err := exec.Command("systemctl", action, svc).CombinedOutput()
	result := map[string]string{"output": string(out)}
	if err != nil {
		result["error"] = err.Error()
	}
	json.NewEncoder(w).Encode(result)
}

// ─── Containers ───────────────────────────────────────────────────────────────

type ContainerInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Image  string `json:"image"`
	Status string `json:"status"`
	State  string `json:"state"`
	Ports  string `json:"ports"`
	Engine string `json:"engine"`
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func getDockerContainers(engine, cmd string) []ContainerInfo {
	out, err := exec.Command(cmd, "ps", "-a", "--format",
		"{{.ID}}|{{.Names}}|{{.Image}}|{{.Status}}|{{.State}}|{{.Ports}}").Output()
	if err != nil {
		return nil
	}
	var containers []ContainerInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "|", 6)
		if len(parts) == 6 && parts[0] != "" {
			containers = append(containers, ContainerInfo{
				ID: parts[0], Name: strings.TrimPrefix(parts[1], "/"),
				Image: parts[2], Status: parts[3], State: parts[4], Ports: parts[5],
				Engine: engine,
			})
		}
	}
	return containers
}

func handleContainerList(w http.ResponseWriter, r *http.Request) {
	var all []ContainerInfo
	if commandExists("docker") {
		if _, err := os.Stat("/var/run/docker.sock"); err == nil {
			all = append(all, getDockerContainers("docker", "docker")...)
		}
	}
	if commandExists("podman") {
		all = append(all, getDockerContainers("podman", "podman")...)
	}
	if commandExists("nerdctl") {
		all = append(all, getDockerContainers("containerd", "nerdctl")...)
	}
	if all == nil {
		all = []ContainerInfo{}
	}
	json.NewEncoder(w).Encode(all)
}

func handleContainerAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	var req map[string]string
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	id, action, engine := req["id"], req["action"], req["engine"]
	allowed := map[string]bool{"start": true, "stop": true, "restart": true}
	if !allowed[action] || id == "" || strings.ContainsAny(id, " ;|&`$(){}\\") {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}
	cmd := "docker"
	if engine == "podman" {
		cmd = "podman"
	} else if engine == "containerd" {
		cmd = "nerdctl"
	}
	out, err := exec.Command(cmd, action, id).CombinedOutput()
	result := map[string]string{"output": string(out)}
	if err != nil {
		result["error"] = err.Error()
	}
	json.NewEncoder(w).Encode(result)
}

// ─── Minecraft ────────────────────────────────────────────────────────────────

func handleMcLog(w http.ResponseWriter, r *http.Request) {
	out, err := exec.Command("tail", "-n", "50", logFilePath).Output()
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"log": "Log not accessible: " + err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"log": string(out)})
}

func handleMcCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	var req map[string]string
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	cmd := req["command"]
	if cmd == "" || strings.ContainsAny(cmd, "\n\r") {
		http.Error(w, `{"error":"invalid"}`, http.StatusBadRequest)
		return
	}
	out, err := exec.Command("screen", "-S", "mc-server", "-p", "0", "-X", "stuff", cmd+"\r").CombinedOutput()
	result := map[string]string{"output": string(out)}
	if err != nil {
		result["error"] = err.Error()
	}
	json.NewEncoder(w).Encode(result)
}

// ─── Auth ─────────────────────────────────────────────────────────────────────

func handleLogin(w http.ResponseWriter, r *http.Request) {
	jsonHeader(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	var req map[string]string
	json.NewDecoder(r.Body).Decode(&req)
	if req["token"] == staticToken {
		w.Write([]byte(`{"ok":true}`))
		return
	}
	w.WriteHeader(http.StatusUnauthorized)
	w.Write([]byte(`{"ok":false,"error":"Invalid token"}`))
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	staticToken = os.Getenv("SYSBOARD_TOKEN")
	if staticToken == "" {
		log.Fatal("SYSBOARD_TOKEN is not set; set it in /opt/sysboard/.env and reload the service")
	}

	port := os.Getenv("SYSBOARD_PORT")
	if port == "" {
		port = "8888"
	}
	listenPort = ":" + port

	logFilePath = os.Getenv("SYSBOARD_LOG_PATH")
	if logFilePath == "" {
		logFilePath = "/var/log/bedrock-server.log"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, "./static/index.html")
	})

	mux.HandleFunc("/api/login", handleLogin)
	mux.HandleFunc("/api/metrics", authMiddleware(handleMetrics))
	mux.HandleFunc("/api/processes", authMiddleware(handleProcesses))
	mux.HandleFunc("/api/services", authMiddleware(handleServices))
	mux.HandleFunc("/api/services/action", authMiddleware(handleServiceAction))
	mux.HandleFunc("/api/containers", authMiddleware(handleContainerList))
	mux.HandleFunc("/api/containers/action", authMiddleware(handleContainerAction))
	mux.HandleFunc("/api/mc/log", authMiddleware(handleMcLog))
	mux.HandleFunc("/api/mc/command", authMiddleware(handleMcCommand))

	log.Printf("SysBoard listening on %s", listenPort)
	if err := http.ListenAndServe(listenPort, mux); err != nil {
		log.Fatal(err)
	}
}