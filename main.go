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
	"strconv"
	"strings"
	"time"
)

const (
	listenPort  = ":8888"
	staticToken = "558855Hilmy!75"
	logFilePath = "/var/log/bedrock-server.log"
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
	CPUPercent  float64    `json:"cpu_percent"`
	RAMTotal    uint64     `json:"ram_total"`
	RAMUsed     uint64     `json:"ram_used"`
	RAMFree     uint64     `json:"ram_free"`
	RAMPercent  float64    `json:"ram_percent"`
	Disks       []DiskInfo `json:"disks"`
	Uptime      string     `json:"uptime"`
	LoadAvg     string     `json:"load_avg"`
	Timestamp   string     `json:"timestamp"`
	NetRxBytes  uint64     `json:"net_rx_bytes"`
	NetTxBytes  uint64     `json:"net_tx_bytes"`
	NetRxRate   float64    `json:"net_rx_rate"`
	NetTxRate   float64    `json:"net_tx_rate"`
	CPUTemp     float64    `json:"cpu_temp"`
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
	// Try hwmon first (most systems)
	pattern := "/sys/class/hwmon/hwmon*/temp*_input"
	matches, err := filepath.Glob(pattern)
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
	// Fallback: thermal_zone
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
	total, used, free := getMemInfo()
	ramPct := 0.0
	if total > 0 {
		ramPct = float64(used) / float64(total) * 100
	}
	disks := []DiskInfo{getDiskInfo("/"), getDiskInfo("/home"), getDiskInfo("/var")}

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
		CPUPercent: cpu, RAMTotal: total, RAMUsed: used, RAMFree: free, RAMPercent: ramPct,
		Disks: disks, Uptime: uptime, LoadAvg: loadAvg, Timestamp: time.Now().Format("15:04:05"),
		NetRxBytes: rxBytes, NetTxBytes: txBytes, NetRxRate: rxRate, NetTxRate: txRate,
		CPUTemp: cpuTemp,
	})
}

// ─── Systemd Services (with RAM + Uptime) ─────────────────────────────────────

type ServiceStatus struct {
	Name      string  `json:"name"`
	Active    string  `json:"active"`
	Sub       string  `json:"sub"`
	Desc      string  `json:"desc"`
	MemBytes  uint64  `json:"mem_bytes"`
	UptimeSec int64   `json:"uptime_sec"`
	UptimeStr string  `json:"uptime_str"`
	LoadState string  `json:"load_state"`
}

func getServiceMemAndUptime(name string) (memBytes uint64, uptimeSec int64) {
	// MemoryCurrent
	out, err := exec.Command("systemctl", "show", name+".service", "--property=MemoryCurrent,ActiveEnterTimestamp").Output()
	if err != nil {
		return
	}
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "MemoryCurrent=") {
			val := strings.TrimPrefix(line, "MemoryCurrent=")
			val = strings.TrimSpace(val)
			if val != "[not set]" && val != "" {
				v, err := strconv.ParseUint(val, 10, 64)
				if err == nil {
					memBytes = v
				}
			}
		}
		if strings.HasPrefix(line, "ActiveEnterTimestamp=") {
			ts := strings.TrimPrefix(line, "ActiveEnterTimestamp=")
			ts = strings.TrimSpace(ts)
			if ts != "" && ts != "n/a" {
				// Parse systemd timestamp: "Tue 2026-06-02 10:52:30 WIB"
				// Use systemd-analyze to avoid timezone parsing complexity
				t, err := time.Parse("Mon 2006-01-02 15:04:05 MST", ts)
				if err == nil {
					uptimeSec = int64(time.Since(t).Seconds())
				}
			}
		}
	}
	return
}

func uptimeStr(sec int64) string {
	if sec <= 0 {
		return ""
	}
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	if sec < 3600 {
		return fmt.Sprintf("%dm %ds", sec/60, sec%60)
	}
	if sec < 86400 {
		return fmt.Sprintf("%dh %dm", sec/3600, (sec%3600)/60)
	}
	return fmt.Sprintf("%dd %dh", sec/86400, (sec%86400)/3600)
}

func handleServices(w http.ResponseWriter, r *http.Request) {
	out, err := exec.Command("systemctl", "list-units", "--type=service", "--all", "--no-pager", "--plain", "--no-legend").Output()
	var statuses []ServiceStatus
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) >= 4 {
				name := strings.TrimSuffix(fields[0], ".service")
				active := fields[2]
				sub := fields[3]
				desc := strings.Join(fields[4:], " ")
				mem, upSec := uint64(0), int64(0)
				if active == "active" {
					mem, upSec = getServiceMemAndUptime(name)
				}
				statuses = append(statuses, ServiceStatus{
					Name: name, Active: active, Sub: sub, Desc: desc,
					MemBytes: mem, UptimeSec: upSec, UptimeStr: uptimeStr(upSec),
				})
			}
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

// ─── Container Engines ────────────────────────────────────────────────────────

type ContainerInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	Status  string `json:"status"`
	State   string `json:"state"`
	Ports   string `json:"ports"`
	Engine  string `json:"engine"`
}

type EngineInfo struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Version   string `json:"version"`
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func socketExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func getDockerContainers(engine, cmd string) []ContainerInfo {
	out, err := exec.Command(cmd, "ps", "-a", "--format", "{{.ID}}|{{.Names}}|{{.Image}}|{{.Status}}|{{.State}}|{{.Ports}}").Output()
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

func getNerdctlContainers() []ContainerInfo {
	out, err := exec.Command("nerdctl", "ps", "-a", "--format", "{{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Status}}\t{{.Ports}}").Output()
	if err != nil {
		return nil
	}
	var containers []ContainerInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) >= 4 && parts[0] != "" {
			state := "running"
			if strings.Contains(strings.ToLower(parts[3]), "exited") {
				state = "exited"
			}
			ports := ""
			if len(parts) >= 6 {
				ports = parts[5]
			}
			containers = append(containers, ContainerInfo{
				ID: parts[0], Name: parts[1], Image: parts[2],
				Status: parts[3], State: state, Ports: ports,
				Engine: "containerd/nerdctl",
			})
		}
	}
	return containers
}

func getKubernetesPods() []ContainerInfo {
	var cmd string
	if commandExists("kubectl") {
		cmd = "kubectl"
	} else if commandExists("k3s") {
		cmd = "k3s"
	} else {
		return nil
	}

	var args []string
	if cmd == "k3s" {
		args = []string{"kubectl", "get", "pods", "--all-namespaces", "--no-headers", "-o",
			"custom-columns=NAME:.metadata.name,NS:.metadata.namespace,STATUS:.status.phase,IMAGE:.spec.containers[0].image"}
	} else {
		args = []string{"get", "pods", "--all-namespaces", "--no-headers", "-o",
			"custom-columns=NAME:.metadata.name,NS:.metadata.namespace,STATUS:.status.phase,IMAGE:.spec.containers[0].image"}
	}

	out, err := exec.Command(cmd, args...).Output()
	if err != nil {
		return nil
	}
	var containers []ContainerInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			state := strings.ToLower(fields[2])
			image := ""
			if len(fields) >= 4 {
				image = fields[3]
			}
			containers = append(containers, ContainerInfo{
				ID: fields[0], Name: fields[0] + " (" + fields[1] + ")",
				Image: image, Status: fields[2], State: state, Ports: "",
				Engine: "kubernetes",
			})
		}
	}
	return containers
}

func handleContainerList(w http.ResponseWriter, r *http.Request) {
	var all []ContainerInfo

	// Docker
	if commandExists("docker") && socketExists("/var/run/docker.sock") {
		all = append(all, getDockerContainers("docker", "docker")...)
	}
	// Podman
	if commandExists("podman") {
		all = append(all, getDockerContainers("podman", "podman")...)
	}
	// containerd via nerdctl
	if commandExists("nerdctl") {
		all = append(all, getNerdctlContainers()...)
	}
	// Kubernetes / k3s
	if commandExists("kubectl") || commandExists("k3s") {
		all = append(all, getKubernetesPods()...)
	}

	if all == nil {
		all = []ContainerInfo{}
	}
	json.NewEncoder(w).Encode(all)
}

func handleEngineInfo(w http.ResponseWriter, r *http.Request) {
	engines := []EngineInfo{}

	if commandExists("docker") {
		ver := ""
		out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Output()
		if err == nil {
			ver = strings.TrimSpace(string(out))
		}
		engines = append(engines, EngineInfo{Name: "docker", Available: socketExists("/var/run/docker.sock"), Version: ver})
	}
	if commandExists("podman") {
		ver := ""
		out, err := exec.Command("podman", "version", "--format", "{{.Version}}").Output()
		if err == nil {
			ver = strings.TrimSpace(string(out))
		}
		engines = append(engines, EngineInfo{Name: "podman", Available: true, Version: ver})
	}
	if commandExists("nerdctl") {
		engines = append(engines, EngineInfo{Name: "containerd/nerdctl", Available: true})
	}
	if commandExists("kubectl") {
		ver := ""
		out, err := exec.Command("kubectl", "version", "--client", "--short").Output()
		if err == nil {
			ver = strings.TrimSpace(string(out))
		}
		engines = append(engines, EngineInfo{Name: "kubernetes", Available: true, Version: ver})
	} else if commandExists("k3s") {
		ver := ""
		out, err := exec.Command("k3s", "--version").Output()
		if err == nil {
			ver = strings.Fields(string(out))[0]
		}
		engines = append(engines, EngineInfo{Name: "k3s", Available: true, Version: ver})
	}

	json.NewEncoder(w).Encode(engines)
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

	var cmd string
	switch engine {
	case "podman":
		cmd = "podman"
	case "containerd/nerdctl":
		cmd = "nerdctl"
	default:
		cmd = "docker"
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

// ─── Plugins ──────────────────────────────────────────────────────────────────

type PluginDef struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Desc        string `json:"desc"`
	Installed   bool   `json:"installed"`
	ServiceName string `json:"service_name"`
	CheckCmd    string `json:"check_cmd"`
	InstallCmd  string `json:"install_cmd"`
	Category    string `json:"category"`
}

var pluginCatalog = []PluginDef{
	{ID: "minecraft", Name: "Minecraft Bedrock", Desc: "Bedrock server console + log viewer", CheckCmd: "bedrock_server", ServiceName: "bedrock", Category: "gaming",
		InstallCmd: "curl -fsSL https://raw.githubusercontent.com/hilmyah/bedrock-server/main/install.sh | bash"},
	{ID: "docker", Name: "Docker", Desc: "Container engine (Docker CE)", CheckCmd: "docker", ServiceName: "docker", Category: "containers",
		InstallCmd: "curl -fsSL https://get.docker.com | sh"},
	{ID: "podman", Name: "Podman", Desc: "Rootless container engine", CheckCmd: "podman", ServiceName: "podman", Category: "containers",
		InstallCmd: "apt-get install -y podman"},
	{ID: "k3s", Name: "k3s", Desc: "Lightweight Kubernetes", CheckCmd: "k3s", ServiceName: "k3s", Category: "orchestration",
		InstallCmd: "curl -sfL https://get.k3s.io | sh -"},
	{ID: "nextcloud", Name: "Nextcloud", Desc: "Self-hosted cloud storage", CheckCmd: "nextcloud", ServiceName: "nextcloud", Category: "apps",
		InstallCmd: "snap install nextcloud"},
	{ID: "portainer", Name: "Portainer", Desc: "Docker/k8s web management UI", CheckCmd: "", ServiceName: "", Category: "containers",
		InstallCmd: "docker volume create portainer_data && docker run -d -p 9000:9000 --restart=always -v /var/run/docker.sock:/var/run/docker.sock -v portainer_data:/data portainer/portainer-ce"},
	{ID: "tailscale", Name: "Tailscale", Desc: "Zero-config VPN mesh network", CheckCmd: "tailscale", ServiceName: "tailscaled", Category: "network",
		InstallCmd: "curl -fsSL https://tailscale.com/install.sh | sh"},
	{ID: "cloudflared", Name: "Cloudflared", Desc: "Cloudflare Tunnel agent", CheckCmd: "cloudflared", ServiceName: "cloudflared", Category: "network",
		InstallCmd: "wget https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64.deb && dpkg -i cloudflared-linux-amd64.deb"},
	{ID: "playit", Name: "Playit.gg", Desc: "Free game server tunneling", CheckCmd: "playit", ServiceName: "playit", Category: "network",
		InstallCmd: "curl -SsL https://playit-cloud.github.io/ppa/key.gpg | gpg --dearmor | tee /etc/apt/trusted.gpg.d/playit.gpg >/dev/null && echo 'deb [signed-by=/etc/apt/trusted.gpg.d/playit.gpg] https://playit-cloud.github.io/ppa/data ./' | tee /etc/apt/sources.list.d/playit-cloud.list && apt update && apt install -y playit"},
}

func handlePlugins(w http.ResponseWriter, r *http.Request) {
	result := make([]PluginDef, len(pluginCatalog))
	for i, p := range pluginCatalog {
		p.Installed = false
		if p.CheckCmd != "" {
			p.Installed = commandExists(p.CheckCmd)
		} else if p.ServiceName != "" {
			out, err := exec.Command("systemctl", "is-active", p.ServiceName).Output()
			p.Installed = err == nil && strings.TrimSpace(string(out)) == "active"
		}
		result[i] = p
	}
	json.NewEncoder(w).Encode(result)
}

func handlePluginInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	var req map[string]string
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	id := req["id"]
	if id == "" {
		http.Error(w, `{"error":"missing id"}`, http.StatusBadRequest)
		return
	}
	var plugin *PluginDef
	for i := range pluginCatalog {
		if pluginCatalog[i].ID == id {
			plugin = &pluginCatalog[i]
			break
		}
	}
	if plugin == nil {
		http.Error(w, `{"error":"unknown plugin"}`, http.StatusNotFound)
		return
	}
	// Execute install command in background, stream status
	out, err := exec.Command("bash", "-c", plugin.InstallCmd).CombinedOutput()
	result := map[string]string{"output": string(out), "id": id}
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
	mux.HandleFunc("/api/services", authMiddleware(handleServices))
	mux.HandleFunc("/api/services/action", authMiddleware(handleServiceAction))
	mux.HandleFunc("/api/mc/log", authMiddleware(handleMcLog))
	mux.HandleFunc("/api/mc/command", authMiddleware(handleMcCommand))
	mux.HandleFunc("/api/containers", authMiddleware(handleContainerList))
	mux.HandleFunc("/api/containers/engines", authMiddleware(handleEngineInfo))
	mux.HandleFunc("/api/containers/action", authMiddleware(handleContainerAction))
	mux.HandleFunc("/api/plugins", authMiddleware(handlePlugins))
	mux.HandleFunc("/api/plugins/install", authMiddleware(handlePluginInstall))

	log.Printf("SysBoard v2 listening on %s", listenPort)
	if err := http.ListenAndServe(listenPort, mux); err != nil {
		log.Fatal(err)
	}
}
