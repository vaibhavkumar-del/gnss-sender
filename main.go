package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	AT_PORT     = "/dev/ttyvat0"
	DUMP_FILE   = "/tmp/gnss_dump"
	SERVER_IP   = "104.199.145.69"
	SERVER_PORT = "10196"
	IMEI        = "350435030985778"
	VEHICLE     = "KA1234"
	FIRMWARE    = "V1.0.1"
	GPIO_PATH   = "/sys/class/gpio/gpio42"
	GPIO_EXPORT = "/sys/class/gpio/export"
	BLINK_COUNT = 3
	DIST_TARGET = 100.0 // meters

	QUEUE_DIR    = "/data/gnss_queue" // persistent internal storage
	MAX_PER_FILE = 100                // packets per log file

	BINARY_PATH      = "/data/gnss_sender"
	UPDATE_CHECK_URL = "https://raw.githubusercontent.com/vaibhavkumar-del/gnss-sender/main/version.json"
	UPDATE_INTERVAL  = 30 * time.Minute
)

type versionInfo struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

func checkForUpdate() {
	client := &http.Client{Timeout: 15 * time.Second}

	resp, err := client.Get(UPDATE_CHECK_URL)
	if err != nil {
		fmt.Printf("[UPDATE] Version check failed: %v\n", err)
		return
	}
	defer resp.Body.Close()

	var info versionInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		fmt.Printf("[UPDATE] Bad version.json: %v\n", err)
		return
	}

	if info.Version == FIRMWARE {
		fmt.Printf("[UPDATE] Already on latest (%s)\n", FIRMWARE)
		return
	}

	fmt.Printf("[UPDATE] New version %s available (current %s), downloading...\n", info.Version, FIRMWARE)

	resp2, err := client.Get(info.URL)
	if err != nil {
		fmt.Printf("[UPDATE] Download failed: %v\n", err)
		return
	}
	defer resp2.Body.Close()

	tmpPath := BINARY_PATH + ".new"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		fmt.Printf("[UPDATE] Cannot write temp file: %v\n", err)
		return
	}
	if _, err := io.Copy(f, resp2.Body); err != nil {
		f.Close()
		os.Remove(tmpPath)
		fmt.Printf("[UPDATE] Write failed: %v\n", err)
		return
	}
	f.Close()

	if err := os.Rename(tmpPath, BINARY_PATH); err != nil {
		fmt.Printf("[UPDATE] Replace failed: %v\n", err)
		return
	}

	fmt.Println("[UPDATE] Update applied — exiting for restart")
	os.Exit(0) // systemd Restart=always will launch the new binary
}

// ── GPIO / LED ────────────────────────────────────────────────────────────────

func gpioWrite(path, value string) {
	f, err := os.OpenFile(path, os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(value)
}

func setupGPIO() {
	gpioWrite(GPIO_EXPORT, "42")
	time.Sleep(300 * time.Millisecond)
	gpioWrite(GPIO_PATH+"/direction", "out")
	gpioWrite(GPIO_PATH+"/value", "0")
	fmt.Println("[LED] GPIO 42 ready")
}

func blinkLED(times int) {
	fmt.Printf("[LED] Blinking %d times — 100m reached!\n", times)
	for i := 0; i < times; i++ {
		gpioWrite(GPIO_PATH+"/value", "1")
		time.Sleep(200 * time.Millisecond)
		gpioWrite(GPIO_PATH+"/value", "0")
		time.Sleep(200 * time.Millisecond)
	}
}

// ── Distance (Haversine) ──────────────────────────────────────────────────────

func toDecimalDeg(raw, dir string) float64 {
	// raw format from modem: DDDMM.MMMM or DDMM.MMMM
	if raw == "" {
		return 0
	}
	var deg, min float64
	fmt.Sscanf(raw, "%f", &min)
	// degrees = integer part of (raw / 100)
	deg = math.Trunc(min / 100)
	min = min - deg*100
	dd := deg + min/60.0
	if dir == "S" || dir == "W" {
		dd = -dd
	}
	return dd
}

func haversineMeters(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371000.0 // Earth radius in metres
	toRad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat := toRad(lat2 - lat1)
	dLon := toRad(lon2 - lon1)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(toRad(lat1))*math.Cos(toRad(lat2))*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	return R * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// ── GNSS ──────────────────────────────────────────────────────────────────────

type GNSSData struct {
	LatVal, LatDir string
	LonVal, LonDir string
	Altitude       string
	FixMode        string
	Satellites     string
	Time           string
	Speed          string
	Heading        string
}

func readGNSS() (*GNSSData, error) {
	const nmeaFile = "/tmp/nmea_raw"
	os.Remove(nmeaFile)

	// Run nmea_test_app redirecting output to file
	cmd := exec.Command("sh", "-c", "/usr/bin/nmea_test_app > "+nmeaFile+" 2>&1")
	cmd.Start()

	// Wait 8 seconds for GPS data
	time.Sleep(8 * time.Second)
	exec.Command("sh", "-c", "killall nmea_test_app 2>/dev/null").Run()
	cmd.Wait()

	data, err := os.ReadFile(nmeaFile)
	if err != nil {
		return nil, fmt.Errorf("read nmea: %w", err)
	}

	nmea := string(data)
	fmt.Printf("[GNSS] NMEA output:\n%s\n---\n", nmea)
	return parseNMEA(nmea), nil
}

func parseNMEA(nmea string) *GNSSData {
	g := &GNSSData{}
	g.Speed = "0.0"
	g.Heading = "0.00"
	g.Satellites = "0"

	for _, line := range strings.Split(nmea, "\n") {
		line = strings.TrimSpace(line)
		// Parse RMC: $GPRMC or $GNRMC
		if (strings.HasPrefix(line, "$GPRMC") || strings.HasPrefix(line, "$GNRMC")) && strings.Contains(line, ",A,") {
			f := strings.Split(line, ",")
			if len(f) >= 10 {
				g.Time = parseNMEATime(f[1], f[9])
				g.LatVal = f[3]
				g.LatDir = f[4]
				g.LonVal = f[5]
				g.LonDir = f[6]
				g.Speed = f[7]
				g.Heading = f[8]
			}
		}
		// Parse GGA: $GPGGA or $GNGGA
		if (strings.HasPrefix(line, "$GPGGA") || strings.HasPrefix(line, "$GNGGA")) {
			f := strings.Split(line, ",")
			if len(f) >= 10 {
				if f[6] != "0" && f[6] != "" {
					if g.LatVal == "" {
						g.LatVal = f[2]
						g.LatDir = f[3]
						g.LonVal = f[4]
						g.LonDir = f[5]
					}
					g.Satellites = f[7]
					g.Altitude = f[9]
					g.FixMode = f[6]
				}
			}
		}
	}
	return g
}

func parseNMEATime(hhmmss, ddmmyy string) string {
	if len(hhmmss) < 6 || len(ddmmyy) < 6 {
		now := time.Now().UTC()
		return now.Format("02012006") + "," + now.Format("150405")
	}
	dd := ddmmyy[0:2]
	mm := ddmmyy[2:4]
	yy := "20" + ddmmyy[4:6]
	hh := hhmmss[0:2]
	mi := hhmmss[2:4]
	ss := hhmmss[4:6]
	return dd + mm + yy + "," + hh + mi + ss
}

func extractField(dump, keyword string) string {
	for _, line := range strings.Split(dump, "\n") {
		line = strings.TrimRight(line, "\r\n")
		if strings.Contains(line, keyword) {
			parts := strings.SplitN(line, ": ", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

func parseDump(dump string) *GNSSData {
	g := &GNSSData{}
	g.LatVal = extractField(dump, "GLATONL")
	g.LatDir = extractField(dump, "GLATDIR")
	g.LonVal = extractField(dump, "GLONONL")
	g.LonDir = extractField(dump, "GLONDIR")

	altRaw := extractField(dump, "CGPSGALT")
	if idx := strings.Index(altRaw, ","); idx != -1 {
		g.Altitude = altRaw[:idx]
	} else {
		g.Altitude = altRaw
	}

	g.FixMode = extractField(dump, "GMODE")
	g.Satellites = extractField(dump, "GNSAT")
	g.Time = parseISOTime(extractField(dump, "TIMEST"))

	for _, line := range strings.Split(dump, "\n") {
		if strings.Contains(line, "GPRMC") {
			fields := strings.Split(strings.TrimSpace(line), ",")
			if len(fields) > 9 {
				g.Speed = fields[7]
				g.Heading = fields[8]
			}
			break
		}
	}

	if g.Speed == "" {
		g.Speed = "0.0"
	}
	if g.Heading == "" {
		g.Heading = "0.00"
	}
	if g.Satellites == "" {
		g.Satellites = "0"
	}
	return g
}

func parseISOTime(iso string) string {
	iso = strings.TrimSpace(iso)
	if len(iso) < 19 {
		now := time.Now().UTC()
		return now.Format("02012006") + "," + now.Format("150405")
	}
	return iso[8:10] + iso[5:7] + iso[0:4] + "," + iso[11:13] + iso[14:16] + iso[17:19]
}

// ── Packet ────────────────────────────────────────────────────────────────────

func xorChecksum(s string) string {
	var cs byte
	for _, b := range []byte(s) {
		cs ^= b
	}
	return fmt.Sprintf("%02X", cs)
}

func buildPacket(g *GNSSData) string {
	body := fmt.Sprintf(
		"$,LA5,ITPL,%s,NR,01,L,%s,%s,1,%s,%s,%s,%s,%s,%s,%s,%s,%s,2.99,2.84,airtel,1,1,12.9,3.8,0,17,404,10,0895,0E19230C,09854303,0895,0,0985432A,0895,0,0E8FEA15,0895,0,0,0,0,1100,10,001464,72,",
		FIRMWARE, IMEI, VEHICLE,
		g.Time,
		g.LatVal, g.LatDir,
		g.LonVal, g.LonDir,
		g.Speed, g.Heading,
		g.Satellites, g.Altitude,
	)
	return body + "*" + xorChecksum(body) + "\r\n"
}

// ── Offline queue (NAND flash) ────────────────────────────────────────────────

// QueuePos tracks a position within the rolling log files.
// FileNum is the 8-digit file index; LineNum is lines written/read in that file.
type QueuePos struct {
	FileNum int
	LineNum int
}

func readQueuePos(path string) QueuePos {
	data, err := os.ReadFile(path)
	if err != nil {
		return QueuePos{FileNum: 1}
	}
	var p QueuePos
	fmt.Sscanf(strings.TrimSpace(string(data)), "%d,%d", &p.FileNum, &p.LineNum)
	if p.FileNum < 1 {
		p.FileNum = 1
	}
	return p
}

func writeQueuePos(path string, p QueuePos) {
	os.WriteFile(path, []byte(fmt.Sprintf("%d,%d", p.FileNum, p.LineNum)), 0644)
}

func queueFilePath(n int) string {
	return fmt.Sprintf("%s/%08d.log", QUEUE_DIR, n)
}

// enqueuePacket appends one AIS-140 packet to the flash queue.
// Rotates to a new file every MAX_PER_FILE entries to bound individual file size.
func enqueuePacket(packet string) {
	if err := os.MkdirAll(QUEUE_DIR, 0755); err != nil {
		fmt.Printf("[QUEUE] mkdir: %v\n", err)
		return
	}
	wposPath := QUEUE_DIR + "/write_pos"
	wp := readQueuePos(wposPath)

	f, err := os.OpenFile(queueFilePath(wp.FileNum), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("[QUEUE] open: %v\n", err)
		return
	}
	fmt.Fprintf(f, "%s\n", strings.TrimRight(packet, "\r\n"))
	f.Close()

	wp.LineNum++
	if wp.LineNum >= MAX_PER_FILE {
		wp.FileNum++
		wp.LineNum = 0
	}
	writeQueuePos(wposPath, wp)
	fmt.Printf("[QUEUE] Stored packet (file %08d, %d/%d)\n", wp.FileNum, wp.LineNum, MAX_PER_FILE)
}

func hasQueuedPackets() bool {
	wp := readQueuePos(QUEUE_DIR + "/write_pos")
	sp := readQueuePos(QUEUE_DIR + "/send_pos")
	return sp.FileNum < wp.FileNum || (sp.FileNum == wp.FileNum && sp.LineNum < wp.LineNum)
}

// flushQueue replays buffered packets in FIFO order, deleting log files as they drain.
// Returns true if the queue is fully empty after the call.
func flushQueue() bool {
	sposPath := QUEUE_DIR + "/send_pos"
	for {
		wp := readQueuePos(QUEUE_DIR + "/write_pos")
		sp := readQueuePos(sposPath)

		if sp.FileNum == wp.FileNum && sp.LineNum >= wp.LineNum {
			return true
		}

		data, err := os.ReadFile(queueFilePath(sp.FileNum))
		if err != nil {
			fmt.Printf("[QUEUE] read file %08d: %v\n", sp.FileNum, err)
			return false
		}

		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		sameFile := sp.FileNum == wp.FileNum
		endLine := len(lines)
		if sameFile && wp.LineNum < endLine {
			endLine = wp.LineNum
		}

		for sp.LineNum < endLine {
			raw := strings.TrimSpace(lines[sp.LineNum])
			if raw == "" {
				sp.LineNum++
				continue
			}
			fmt.Printf("[QUEUE] Replaying file %08d line %d\n", sp.FileNum, sp.LineNum)
			if !sendToServer(raw + "\r\n") {
				writeQueuePos(sposPath, sp)
				return false
			}
			sp.LineNum++
			writeQueuePos(sposPath, sp)
		}

		if !sameFile {
			os.Remove(queueFilePath(sp.FileNum))
			fmt.Printf("[QUEUE] Deleted drained file %08d\n", sp.FileNum)
			sp.FileNum++
			sp.LineNum = 0
			writeQueuePos(sposPath, sp)
			// continue loop to drain the next file
		} else {
			return true
		}
	}
}

// ── Server send ───────────────────────────────────────────────────────────────

func sendToServer(packet string) bool {
	address := SERVER_IP + ":" + SERVER_PORT
	fmt.Printf("[NET] Connecting to %s...\n", address)

	conn, err := net.DialTimeout("tcp", address, 10*time.Second)
	if err != nil {
		fmt.Printf("[NET] Connect failed: %v\n", err)
		return false
	}
	defer conn.Close()

	_, err = conn.Write([]byte(packet))
	if err != nil {
		fmt.Printf("[NET] Send failed: %v\n", err)
		return false
	}
	fmt.Println("[NET] Packet sent!")

	go func(c net.Conn) {
		reader := bufio.NewReader(c)
		for {
			line, err := reader.ReadString('\n')
			if len(line) > 0 {
				fmt.Printf("[SERVER] %s\n", line)
			}
			if err != nil {
				return
			}
		}
	}(conn)

	time.Sleep(2 * time.Second)
	return true
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	setupGPIO()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	var (
		anchorLat, anchorLon float64 // last blink point
		accumulated          float64 // metres since last blink
		hasAnchor            bool    // do we have a valid first fix?
		prevLat, prevLon     float64 // previous reading for incremental distance
		hasPrev              bool
	)

	fmt.Println("[*] Starting — will blink LED every 100m travelled")
	fmt.Println("[*] Waiting 30s for system and GPS stack to initialize...")
	time.Sleep(30 * time.Second)
	fmt.Println("[*] Starting GPS loop")

	checkForUpdate()
	lastUpdateCheck := time.Now()

	for {
		select {
		case <-sig:
			fmt.Println("\n[*] Exiting.")
			gpioWrite(GPIO_PATH+"/value", "0")
			os.Exit(0)
		default:
		}

		if time.Since(lastUpdateCheck) >= UPDATE_INTERVAL {
			checkForUpdate()
			lastUpdateCheck = time.Now()
		}

		fmt.Println("[*] Reading GNSS...")
		g, err := readGNSS()
		if err != nil {
			fmt.Printf("[!] GNSS error: %v — retrying in 5s\n", err)
			time.Sleep(5 * time.Second)
			continue
		}

		curLat := toDecimalDeg(g.LatVal, g.LatDir)
		curLon := toDecimalDeg(g.LonVal, g.LonDir)

		fmt.Printf("[GNSS] LAT:%.6f %s | LON:%.6f %s | ALT:%s | SPD:%s | HDG:%s | SAT:%s | FIX:%s | TIME:%s\n",
			curLat, g.LatDir, curLon, g.LonDir,
			g.Altitude, g.Speed, g.Heading, g.Satellites, g.FixMode, g.Time,
		)

		validFix := g.Satellites != "0" && g.LatVal != "" && g.LonVal != ""

		if validFix {
			if !hasAnchor {
				anchorLat, anchorLon = curLat, curLon
				prevLat, prevLon = curLat, curLon
				hasAnchor = true
				hasPrev = true
				fmt.Printf("[DIST] Anchor set at %.6f, %.6f\n", anchorLat, anchorLon)
			} else if hasPrev {
				delta := haversineMeters(prevLat, prevLon, curLat, curLon)
				accumulated += delta
				fmt.Printf("[DIST] +%.1fm this step | %.1f / %.0fm total since last blink\n",
					delta, accumulated, DIST_TARGET)

				if accumulated >= DIST_TARGET {
					go blinkLED(BLINK_COUNT)
					fmt.Printf("[DIST] 100m reached! Resetting anchor. (Anchor was %.6f,%.6f)\n",
						anchorLat, anchorLon)
					anchorLat, anchorLon = curLat, curLon
					accumulated = 0
				}

				prevLat, prevLon = curLat, curLon
			}
		} else {
			fmt.Println("[DIST] No valid fix — skipping distance update")
		}

		packet := buildPacket(g)
		fmt.Printf("[NET] Packet: %s", packet)

		// Drain any buffered packets first to maintain FIFO delivery order.
		// Only send the live packet if the queue is fully empty.
		allFlushed := true
		if hasQueuedPackets() {
			fmt.Println("[QUEUE] Offline packets pending — flushing before live send...")
			allFlushed = flushQueue()
		}

		if allFlushed {
			if !sendToServer(packet) {
				fmt.Println("[QUEUE] Send failed — buffering to flash")
				enqueuePacket(packet)
			}
		} else {
			fmt.Println("[QUEUE] Queue not fully drained — buffering live packet")
			enqueuePacket(packet)
		}

		time.Sleep(10 * time.Second)
	}
}
