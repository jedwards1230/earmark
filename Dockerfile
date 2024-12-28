FROM golang:1.21-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o /transcriber ./cmd/main.go

FROM python:3.9-slim

WORKDIR /
COPY --from=builder /transcriber /transcriber

RUN apt-get update && apt-get install -y \
    gcc g++ make cmake \
    git \
    && rm -rf /var/lib/apt/lists/*

RUN python3 -m venv /venv
ENV VIRTUAL_ENV=/venv
ENV PATH="$VIRTUAL_ENV/bin:$PATH"
ENV PYTHONPATH="$VIRTUAL_ENV/lib/python3.9/site-packages:$PYTHONPATH"

RUN pip install --no-cache-dir --upgrade pip wheel setuptools
RUN pip install --no-cache-dir ctranslate2 whisper-ctranslate2

ENTRYPOINT ["/transcriber"]