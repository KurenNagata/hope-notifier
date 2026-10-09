# ---- build ----
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/hope-notifier . && mkdir -p /out/data

# ---- run ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build --chown=65532:65532 /out/data /data
WORKDIR /data
COPY --from=build /out/hope-notifier /usr/local/bin/hope-notifier
ENV STATE_FILE=/data/state.json
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/hope-notifier"]
