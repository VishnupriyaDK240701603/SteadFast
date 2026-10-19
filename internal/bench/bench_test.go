package bench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollectSystemInfo(t *testing.T) {
	info := CollectSystemInfo()
	if info.OS == "" {
		t.Fatalf("expected OS to be populated")
	}
	if info.CPUCores <= 0 {
		t.Fatalf("expected CPUCores > 0, got %d", info.CPUCores)
	}
	if info.RAMTotalBytes <= 0 {
		t.Fatalf("expected RAMTotalBytes > 0, got %d", info.RAMTotalBytes)
	}
	if info.WSLMemoryCap == "" {
		t.Fatalf("expected WSLMemoryCap to be populated")
	}
}

func TestArtifactGenerators(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "bench_test_*")
	if err != nil {
		t.Fatalf("failed to create tmp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	csvPath := filepath.Join(tmpDir, "chaos_throughput.csv")
	if err := writeChaosThroughputCSV(csvPath); err != nil {
		t.Fatalf("failed to write csv: %v", err)
	}
	csvData, err := os.ReadFile(csvPath)
	if err != nil {
		t.Fatalf("failed to read csv: %v", err)
	}
	if !strings.Contains(string(csvData), "second,steadfast_throughput_rps") {
		t.Fatalf("expected csv header in output")
	}

	svgPath := filepath.Join(tmpDir, "chaos_throughput.svg")
	if err := writeChaosThroughputSVG(svgPath); err != nil {
		t.Fatalf("failed to write svg: %v", err)
	}
	svgData, err := os.ReadFile(svgPath)
	if err != nil {
		t.Fatalf("failed to read svg: %v", err)
	}
	if !strings.Contains(string(svgData), "<svg") || !strings.Contains(string(svgData), "</svg>") {
		t.Fatalf("expected valid svg format")
	}
}
