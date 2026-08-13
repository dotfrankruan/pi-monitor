package metrics

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

type SystemInfo struct {
	Hostname          string                 `json:"hostname"`
	OperatingSystem   string                 `json:"operating_system"`
	KernelVersion     string                 `json:"kernel_version"`
	Architecture      string                 `json:"architecture"`
	Model             string                 `json:"model,omitempty"`
	CPUCores          int                    `json:"cpu_cores"`
	DiskDevice        string                 `json:"disk_device,omitempty"`
	DiskFilesystem    string                 `json:"disk_filesystem,omitempty"`
	DiskMount         string                 `json:"disk_mount"`
	HasFan            bool                   `json:"has_fan"`
	HasNVMeTemp       bool                   `json:"has_nvme_temp"`
	NetworkInterfaces []NetworkInterfaceInfo `json:"network_interfaces"`
}

type NetworkInterfaceInfo struct {
	Name      string   `json:"name"`
	State     string   `json:"state"`
	MAC       string   `json:"mac,omitempty"`
	MTU       int      `json:"mtu"`
	Addresses []string `json:"addresses"`
}

func (c *Collector) SystemInfo() SystemInfo {
	hostname, _ := os.Hostname()
	info := SystemInfo{
		Hostname: hostname, Architecture: runtime.GOARCH, CPUCores: runtime.NumCPU(),
		DiskMount: c.diskPath, HasFan: c.fanRPMPath != "", HasNVMeTemp: c.nvmeTempPath != "",
	}
	if text, err := readText(filepath.Join(c.procRoot, "sys/kernel/osrelease")); err == nil {
		info.KernelVersion = strings.TrimSpace(text)
	}
	if text, err := readText(filepath.Join(c.sysRoot, "firmware/devicetree/base/model")); err == nil {
		info.Model = strings.Trim(strings.TrimSpace(text), "\x00")
	}
	info.OperatingSystem = readOSName(c.procRoot)
	info.DiskDevice, info.DiskFilesystem, info.DiskMount = mountInfo(c.procRoot, c.diskPath)
	info.NetworkInterfaces = networkInterfaces(c.sysRoot)
	return info
}

func networkInterfaces(sysRoot string) []NetworkInterfaceInfo {
	interfaces, _ := net.Interfaces()
	result := make([]NetworkInterfaceInfo, 0, len(interfaces))
	for _, iface := range interfaces {
		info := NetworkInterfaceInfo{Name: iface.Name, MAC: iface.HardwareAddr.String(), MTU: iface.MTU}
		if state, err := readText(filepath.Join(sysRoot, "class/net", iface.Name, "operstate")); err == nil {
			info.State = strings.TrimSpace(state)
		}
		if addresses, err := iface.Addrs(); err == nil {
			for _, address := range addresses {
				info.Addresses = append(info.Addresses, address.String())
			}
		}
		result = append(result, info)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func readOSName(procRoot string) string {
	paths := []string{"/etc/os-release"}
	if procRoot != "/proc" {
		paths = []string{filepath.Join(filepath.Dir(procRoot), "etc/os-release")}
	}
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "PRETTY_NAME=") {
				value := strings.TrimPrefix(scanner.Text(), "PRETTY_NAME=")
				file.Close()
				if unquoted, err := strconv.Unquote(value); err == nil {
					return unquoted
				}
				return strings.Trim(value, "\"")
			}
		}
		file.Close()
	}
	return runtime.GOOS
}

func mountInfo(procRoot, target string) (device, filesystem, mount string) {
	mount = target
	file, err := os.Open(filepath.Join(procRoot, "mounts"))
	if err != nil {
		return
	}
	defer file.Close()
	bestLength := -1
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		candidate := unescapeMount(fields[1])
		if (target == candidate || strings.HasPrefix(target, strings.TrimSuffix(candidate, "/")+"/")) && len(candidate) > bestLength {
			device, filesystem, mount = unescapeMount(fields[0]), fields[2], candidate
			bestLength = len(candidate)
		}
	}
	return
}

func unescapeMount(value string) string {
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(value)
}
