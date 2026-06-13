// Package transcribe — engine.go is intentionally minimal.
// The EngineManager abstraction is removed; transcription is handled by the
// external Python ASR runner (NeMo Parakeet) via the transcription_jobs queue.
package transcribe
