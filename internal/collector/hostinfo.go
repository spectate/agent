package collector

import (
	"context"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/host"
	pb "github.com/spectate/agent/v2/internal/proto/agentv2"
)

// HostInfo gathers the host identity with the same keys the v1 agent sent as
// `host.info` / `host.cpus` INFO metrics.
func HostInfo(ctx context.Context) (*pb.HostInfo, error) {
	h, err := host.InfoWithContext(ctx)
	if err != nil {
		return nil, err
	}
	info := &pb.HostInfo{Info: labels(
		"hostname", h.Hostname,
		"os", h.OS,
		"platform", h.Platform,
		"platform_family", h.PlatformFamily,
		"platform_version", h.PlatformVersion,
		"kernel_version", h.KernelVersion,
		"kernel_arch", h.KernelArch,
		"virtualization_system", h.VirtualizationSystem,
		"virtualization_role", h.VirtualizationRole,
		"host_id", h.HostID,
	)}

	cpus, err := cpu.InfoWithContext(ctx)
	if err == nil {
		for _, c := range cpus {
			info.Cpus = append(info.Cpus, &pb.HostInfo_Cpu{Info: labels(
				"vendor_id", c.VendorID,
				"family", c.Family,
				"model", c.Model,
				"stepping", strconv.Itoa(int(c.Stepping)),
				"physical_id", c.PhysicalID,
				"core_id", c.CoreID,
				"cores", strconv.Itoa(int(c.Cores)),
				"model_name", c.ModelName,
				"mhz", strconv.Itoa(int(c.Mhz)),
				"cache_size", strconv.Itoa(int(c.CacheSize)),
				"flags", strings.Join(c.Flags, ","),
				"microcode", c.Microcode,
			)})
		}
	}
	return info, nil
}

// HostInfoMaps converts host info to the JSON shape of the register endpoint.
func HostInfoMaps(info *pb.HostInfo) (map[string]string, []map[string]string) {
	toMap := func(ls []*pb.Label) map[string]string {
		m := make(map[string]string, len(ls))
		for _, l := range ls {
			m[l.Key] = l.Value
		}
		return m
	}
	var cpus []map[string]string
	for _, c := range info.GetCpus() {
		cpus = append(cpus, toMap(c.Info))
	}
	return toMap(info.GetInfo()), cpus
}

// Hostname returns the hostname from host info.
func Hostname(info *pb.HostInfo) string {
	for _, l := range info.GetInfo() {
		if l.Key == "hostname" {
			return l.Value
		}
	}
	return ""
}

func labels(kv ...string) []*pb.Label {
	out := make([]*pb.Label, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, &pb.Label{Key: kv[i], Value: kv[i+1]})
	}
	return out
}
