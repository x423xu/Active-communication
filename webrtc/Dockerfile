FROM golang:1.22-bookworm AS build
WORKDIR /app

# Copy BOTH module file and source so "go mod tidy" sees imports.
COPY go.mod main.go ./

# Debug: show what Docker actually received
RUN echo "=== go.mod ===" && cat go.mod && \
    echo "=== main.go first lines ===" && head -n 20 main.go

# Force module mode and fetch deps (generates go.sum inside container)
ENV GO111MODULE=on
RUN go mod tidy && go mod download

# Build
RUN CGO_ENABLED=0 GOOS=linux go build -o server .

FROM debian:bookworm-slim
WORKDIR /app
COPY --from=build /app/server /app/server
EXPOSE 8765
CMD ["/app/server"]
