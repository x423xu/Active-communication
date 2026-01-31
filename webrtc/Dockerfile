FROM golang:1.22-bookworm AS build
WORKDIR /app

COPY go.mod main.go ./

ENV GO111MODULE=on
RUN go mod tidy && go mod download

RUN CGO_ENABLED=0 GOOS=linux go build -o server .

FROM debian:bookworm-slim
WORKDIR /app

# ffmpeg for decoding VP8 RTP -> JPEG frames
RUN apt-get update && apt-get install -y --no-install-recommends \
      ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /app/server /app/server
EXPOSE 8765
CMD ["/app/server"]