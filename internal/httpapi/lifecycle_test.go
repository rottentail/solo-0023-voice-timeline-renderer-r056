package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/service"
	"voice-timeline-renderer/internal/storage"
)

func sha256Text(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func newServerWithData(t *testing.T, dataDir string) (*httptest.Server, *service.Service, *storage.Repository) {
	t.Helper()
	repo, err := storage.NewRepository(dataDir)
	if err != nil {
		t.Fatalf("repository: %v", err)
	}
	svc := service.New(repo, service.DefaultConfig())
	server := httptest.NewServer(New(svc, Options{MaxSourceRequestBytes: 48 << 20, MaxJSONBytes: 2 << 20}).Handler())
	t.Cleanup(server.Close)
	return server, svc, repo
}

func putJSON(t *testing.T, url string, body any) (int, map[string]any) {
	t.Helper()
	payload, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("build request %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request %s: %v", url, err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

func stereoWAVPayload(t *testing.T, frames int, left, right float32) string {
	t.Helper()
	samples := make([]float32, frames*2)
	for f := 0; f < frames; f++ {
		samples[f*2] = left
		samples[f*2+1] = right
	}
	wav, err := audiocodec.EncodePCM16(48000, 2, samples)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return base64.StdEncoding.EncodeToString(wav)
}

// Full HTTP lifecycle: a legacy anti-phase record, an idempotent re-upload
// that surfaces corrected readings, and a render that keeps channel balance
// while reaching the whole-work peak target.
func TestHTTPLegacyHealingAndRender(t *testing.T) {
	dataDir := t.TempDir()
	server, _, repo := newServerWithData(t, dataDir)
	base := server.URL

	// Seed a legacy source directly on disk, carrying near-silence readings.
	samples := make([]float32, 480*2)
	for f := 0; f < 480; f++ {
		samples[f*2], samples[f*2+1] = 0.5, -0.5
	}
	wav, err := audiocodec.EncodePCM16(48000, 2, samples)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(dataDir, "sources", "legacy_http")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source.wav"), wav, 0o640); err != nil {
		t.Fatal(err)
	}
	legacyMeta := map[string]any{
		"id": "legacy_http", "name": "legacy legacy_http", "digest": sha256Text(wav),
		"byte_count": len(wav), "sample_rate": 48000, "channels": 2, "bits_per_sample": 16,
		"frame_count": 480, "duration_ms": 10, "peak_dbfs": -120, "rms_dbfs": -120,
		"created_at": "2026-11-01T00:00:00Z",
	}
	rawMeta, _ := json.Marshal(legacyMeta)
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), rawMeta, 0o640); err != nil {
		t.Fatal(err)
	}

	// First GET lazily heals the record through the real HTTP handler.
	status, body := getJSON(t, base+"/v1/sources/legacy_http")
	if status != 200 {
		t.Fatalf("legacy GET status=%d body=%v", status, body)
	}
	wantPeak := 20 * math.Log10(0.5)
	if peak, _ := body["peak_dbfs"].(float64); math.Abs(peak-wantPeak) > 0.02 {
		t.Fatalf("healed GET peak=%v, want %.3f", body["peak_dbfs"], wantPeak)
	}
	if version, _ := body["measurements_version"].(float64); version != 2 {
		t.Fatalf("healed GET measurements_version=%v, want 2", body["measurements_version"])
	}

	// Re-uploading identical bytes remains idempotent (200, no conflict) and
	// keeps returning the corrected readings.
	status, body = putJSON(t, base+"/v1/sources/legacy_http", map[string]any{
		"name": "legacy legacy_http", "wav_base64": base64.StdEncoding.EncodeToString(wav),
	})
	if status != 200 {
		t.Fatalf("idempotent re-upload status=%d body=%v", status, body)
	}
	if peak, _ := body["peak_dbfs"].(float64); math.Abs(peak-wantPeak) > 0.02 {
		t.Fatalf("re-upload peak=%v, want %.3f", body["peak_dbfs"], wantPeak)
	}

	// Register an unbalanced stereo source and render both together.
	status, unbalanced := putJSON(t, base+"/v1/sources/unbalanced_http", map[string]any{
		"name": "unbalanced", "wav_base64": stereoWAVPayload(t, 480, 0.4, 0.1),
	})
	if status != 201 {
		t.Fatalf("register unbalanced status=%d body=%v", status, unbalanced)
	}
	if peak, _ := unbalanced["peak_dbfs"].(float64); math.Abs(peak-20*math.Log10(0.4)) > 0.02 {
		t.Fatalf("unbalanced registered peak=%v", unbalanced["peak_dbfs"])
	}

	status, timeline := putJSON(t, base+"/v1/timelines/tl_http", map[string]any{
		"name": "http timeline", "sample_rate": 48000, "output_channels": 2, "target_peak_dbfs": -1,
		"spans": []map[string]any{
			{"source_id": "legacy_http", "source_start_frame": 0, "source_end_frame": 480, "gain_db": 0, "crossfade_frames": 0},
			{"source_id": "unbalanced_http", "source_start_frame": 0, "source_end_frame": 480, "gain_db": 0, "crossfade_frames": 120},
		},
	})
	if status != 201 {
		t.Fatalf("create timeline status=%d body=%v", status, timeline)
	}
	status, job := putJSON(t, base+"/v1/renders/job_http", map[string]any{"timeline_id": "tl_http"})
	if status != 201 || job["state"] != "completed" {
		t.Fatalf("render status=%d job=%v", status, job)
	}
	status, receipt := getJSON(t, base+"/v1/renders/job_http/receipt")
	if status != 200 {
		t.Fatalf("receipt status=%d body=%v", status, receipt)
	}
	outputPeak, _ := receipt["normalization"].(map[string]any)["output_peak_dbfs"].(float64)
	if math.Abs(outputPeak-(-1.0)) > 0.25 {
		t.Fatalf("receipt output peak %.3f does not meet -1 dBFS target", outputPeak)
	}

	resp, err := http.Get(base + "/v1/renders/job_http/output")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("output status=%d", resp.StatusCode)
	}
	outputWAV := make([]byte, 1<<16)
	n, _ := resp.Body.Read(outputWAV)
	clip, _, err := audiocodec.DecodeWAV(outputWAV[:n])
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	// In a region with only the unbalanced span (after its crossfade), the 4:1
	// channel relationship must survive.
	soloFrame := int64(480 + 300)
	left := float64(clip.Samples[soloFrame*2])
	right := float64(clip.Samples[soloFrame*2+1])
	if math.Abs(right/left-0.25) > 1e-3 {
		t.Fatalf("rendered R/L ratio=%.4f, want 0.25", right/left)
	}

	// Simulating a service restart: the backfill must find nothing left to do.
	repaired, err := repo.BackfillSourceMeasurements(context.Background())
	if err != nil || repaired != 0 {
		t.Fatalf("post-restart backfill = (%d, %v), want 0", repaired, err)
	}
}
