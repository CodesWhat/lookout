package protocol

import (
	"encoding/json"
	"testing"
)

// FuzzEnvelope feeds arbitrary bytes to the wire envelope parser and verifies
// that it never panics. The property under test is "no panic / graceful error":
// both the outer Envelope unmarshal and any inner typed-payload unmarshal must
// return without panicking on arbitrary input.
func FuzzEnvelope(f *testing.F) {
	// Seed valid envelopes for every message type that carries a typed payload.
	f.Add([]byte(`{"type":"request","data":{"requestId":"r1","method":"GET","path":"/containers/json"}}`))
	f.Add([]byte(`{"type":"exec_start","data":{"execId":"e1","containerId":"c1","cmd":["sh"],"cols":80,"rows":24}}`))
	f.Add([]byte(`{"type":"exec_input","data":{"execId":"e1","data":"aGVsbG8="}}`))
	f.Add([]byte(`{"type":"exec_resize","data":{"execId":"e1","cols":120,"rows":40}}`))
	f.Add([]byte(`{"type":"exec_end","data":{"execId":"e1","reason":"done"}}`))
	f.Add([]byte(`{"type":"ping","data":{"timestamp":1700000000}}`))
	f.Add([]byte(`{"type":"metrics","data":{"cpuUsage":0.5,"cpuCores":4,"memoryTotal":8589934592,"memoryUsed":4294967296,"memoryFree":4294967296,"diskTotal":107374182400,"diskUsed":53687091200,"diskFree":53687091200,"diskMetricsAvailable":true,"diskError":"","networkRxBytes":1024,"networkTxBytes":512,"uptime":3600}}`))
	f.Add([]byte(`{"type":"hello","data":{"version":"1.0","protocol":"portwing/1.0","agentId":"a1","agentName":"test","dockerVersion":"24.0","hostname":"host","capabilities":["exec"]}}`))
	f.Add([]byte(`{"type":"response","data":{"requestId":"r1","statusCode":200}}`))
	f.Add([]byte(`{"type":"stream","data":{"requestId":"r1","data":"aGVsbG8="}}`))
	f.Add([]byte(`{"type":"stream_end","data":{"requestId":"r1","reason":"eof"}}`))
	f.Add([]byte(`{"type":"error","data":{"message":"something broke","code":"ERR_INTERNAL"}}`))
	f.Add([]byte(`{"type":"dd:watch_request","data":{"watcherType":"docker","watcherName":"main"}}`))
	f.Add([]byte(`{"type":"dd:trigger_request","data":{"triggerType":"restart","triggerName":"web","containerId":"c1"}}`))
	f.Add([]byte(`{"type":"dd:container_log_request","data":{"containerId":"c1","tail":100}}`))
	f.Add([]byte(`{"type":"dd:container_delete_request","data":{"containerId":"c1"}}`))
	// Welcome seeds: TypeWelcome is otherwise unreached by any seed above, and
	// (*WelcomeMessage).UnmarshalJSON is the only statement-bearing function
	// in this package, so FuzzEnvelope scored 0.00% coverage without these.
	// Covers every PollInterval shape UnmarshalJSON branches on: a plain
	// number, a numeric string, a non-numeric string, a non-number/non-string
	// JSON value, an absent field, and a data payload that isn't an object.
	f.Add([]byte(`{"type":"welcome","data":{"pollInterval":30,"config":{"k":"v"},"capabilities":["response_body_base64"]}}`))
	f.Add([]byte(`{"type":"welcome","data":{"pollInterval":"30"}}`))
	f.Add([]byte(`{"type":"welcome","data":{"pollInterval":"abc"}}`))
	f.Add([]byte(`{"type":"welcome","data":{"pollInterval":true}}`))
	f.Add([]byte(`{"type":"welcome","data":{}}`))
	f.Add([]byte(`{"type":"welcome","data":[1,2]}`))
	// Hostile / edge-case seeds.
	f.Add([]byte(``))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"type":""}`))
	f.Add([]byte(`{"type":"exec_start","data":null}`))
	f.Add([]byte(`{"type":"exec_start","data":"not an object"}`))
	f.Add([]byte(`not json at all`))
	f.Add([]byte(`{"type":"ping","data":{"timestamp":"not-a-number"}}`))
	f.Add([]byte("\x00\x01\x02\x03"))

	f.Fuzz(func(t *testing.T, b []byte) {
		var env Envelope
		if err := json.Unmarshal(b, &env); err != nil {
			// Unmarshal errors are fine — the important thing is no panic.
			return
		}

		// When the envelope decoded successfully, attempt to decode the typed
		// inner payload for every known type that carries one.
		switch env.Type {
		case TypeRequest:
			var m RequestMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeResponse:
			var m ResponseMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeStream:
			var m StreamMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeStreamEnd:
			var m StreamEndMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeExecStart:
			var m ExecStartMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeExecReady:
			var m ExecReadyMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeExecInput:
			var m ExecInputMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeExecOutput:
			var m ExecOutputMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeExecResize:
			var m ExecResizeMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeExecEnd:
			var m ExecEndMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeMetrics:
			var m MetricsMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeContainerEvent:
			var m ContainerEventMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeError:
			var m ErrorMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypePing:
			var m PingMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypePong:
			var m PongMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeHello:
			var m HelloMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeWelcome:
			var m WelcomeMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeDDWatchRequest:
			var m DDWatchRequestMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeDDWatchResponse:
			var m DDWatchResponseMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeDDWatchContainerRequest:
			var m DDWatchContainerRequestMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeDDWatchContainerResponse:
			var m DDWatchContainerResponseMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeDDTriggerRequest:
			var m DDTriggerRequestMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeDDTriggerResponse:
			var m DDTriggerResponseMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeDDContainerLogRequest:
			var m DDContainerLogRequestMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeDDContainerLogResponse:
			var m DDContainerLogResponseMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeDDContainerLogChunk:
			var m DDContainerLogChunkMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeDDContainerLogEnd:
			var m DDContainerLogEndMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeDDContainerLogError:
			var m DDContainerLogErrorMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeDDContainerLogCancel:
			var m DDContainerLogCancelMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeDDContainerDeleteRequest:
			var m DDContainerDeleteRequestMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeDDContainerDeleteResponse:
			var m DDContainerDeleteResponseMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeDDContainerSync:
			var m DDContainerSyncMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeDDContainerAdded:
			var m DDContainerAddedMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeDDContainerUpdated:
			var m DDContainerUpdatedMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeDDContainerRemoved:
			var m DDContainerRemovedMessage
			_ = json.Unmarshal(env.Data, &m)
		case TypeDDComponentSync:
			var m DDComponentSyncMessage
			_ = json.Unmarshal(env.Data, &m)
		}
		// Unknown types: the envelope decoded fine; nothing else to assert.
	})
}
