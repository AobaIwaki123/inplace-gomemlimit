# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS builder

WORKDIR /workspace

COPY go.mod go.sum* ./
RUN go mod download || true

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w -extldflags '-static'" \
    -trimpath \
    -o /workspace/bin/demo-app ./cmd/demo-app

FROM gcr.io/distroless/static:nonroot

WORKDIR /
COPY --from=builder /workspace/bin/demo-app /demo-app
USER 65532:65532

ENTRYPOINT ["/demo-app"]
