# Audiobook Transcription Service

This is a personal service to automatically transcribe audiobooks using a Whisper service.

## Overview

The service monitors a specified directory for new audio files. When a new audio file is detected, it is added to a queue for transcription. A worker process takes audio files from the queue, transcribes them using a Dockerized Whisper service, and saves the resulting text files to a separate directory.

The service uses file-based state management to avoid redundant transcriptions.

## Components

*   **Directory Monitoring:** Uses `fsnotify` to watch for new audio files in the input directory.
*   **Queue Management:** Uses a simple in-memory channel to queue audio files for processing.
*   **State Management:** Uses a JSON file to track which audio files have been transcribed.
*   **Transcription:** Uses a Docker container running `ggerganov/whisper.cpp` via the `go-whisper` image to transcribe audio files.

## Dependencies

*   **Go:** The service is written in Go.
*   **fsnotify:** For file system monitoring.
*   **Docker:** To run the Whisper transcription service.
*   `go-whisper` Docker Image: <source_id data="https://github.com/appleboy/go-whisper" />(https://github.com/appleboy/go-whisper)

## Configuration

The service is configured using a `config.json` file:

```json
{
  "audio_dir": "/path/to/audiobooks",
  "output_dir": "/path/to/transcriptions",
  "state_file": "transcriber_state.json",
  "whisper_model": "/models/ggml-small.bin",
  "whisper_docker_image": "ghcr.io/appleboy/go-whisper:latest"
}
```

*   `audio_dir`: The directory to monitor for new audio files.
*   `output_dir`: The directory where transcribed text files will be saved.
*   `state_file`: The JSON file used to track processed files.
*   `whisper_model`: Path to the whisper model file.
*   `whisper_docker_image`: The Docker image to use for transcription.

## Usage

1.  **Build the Docker image:**
    ```bash
    docker build -t transcriber .
    ```
2.  **Run the Docker container:**
    ```bash
    docker run -v /path/to/config.json:/app/config.json -v /path/to/audiobooks:/audiobooks -v /path/to/transcriptions:/transcriptions -v /path/to/models:/models transcriber
    ```
    *   Replace `/path/to/config.json`, `/path/to/audiobooks`, `/path/to/transcriptions` and `/path/to/models` with the actual paths on your system.
3.  The service will automatically start monitoring the specified directory and transcribing new audio files.

## State Management

The service stores the list of processed files in a JSON file specified by `state_file` in the `config.json`.  This file is read on start, and updated on each successful transcription to prevent redundant transcriptions.

## Notes

*   The service uses a buffered channel for the work queue, allowing a maximum of 100 items in the queue.
*   The service uses the `go-whisper` Docker image for transcription. Refer to the [go-whisper repository](https://github.com/appleboy/go-whisper) for more information on available models and options.
*   The service logs all actions and errors to the standard output.