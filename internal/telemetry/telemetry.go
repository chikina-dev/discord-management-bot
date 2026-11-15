package telemetry

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

type Meter struct {
	mu     sync.Mutex
	writer io.Writer
}

func New(writer io.Writer) *Meter {
	if writer == nil {
		writer = os.Stdout
	}
	return &Meter{writer: writer}
}
func (m *Meter) Emit(event string, fields map[string]any, metrics map[string]float64) {
	if m == nil {
		return
	}
	obj := map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano), "event": event, "Service": "discord-management-bot"}
	for k, v := range fields {
		obj[k] = v
	}
	if len(metrics) > 0 {
		definitions := make([]map[string]string, 0, len(metrics))
		for k, v := range metrics {
			obj[k] = v
			definitions = append(definitions, map[string]string{"Name": k})
		}
		obj["_aws"] = map[string]any{"Timestamp": time.Now().UnixMilli(), "CloudWatchMetrics": []any{map[string]any{"Namespace": "DiscordManagementGateway", "Dimensions": [][]string{{"Service"}}, "Metrics": definitions}}}
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return
	}
	data = append(data, '\n')
	m.mu.Lock()
	defer m.mu.Unlock()
	_, _ = m.writer.Write(data)
}
