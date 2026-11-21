FROM golang:1.27.1-bookworm AS build
WORKDIR /src
RUN apt-get update && apt-get install -y --no-install-recommends libbluetooth-dev libdbus-1-dev pkg-config && rm -rf /var/lib/apt/lists/*
COPY go.mod go.sum ./
RUN go mod download
COPY main.go dashboard.go ./
COPY web ./web
RUN CGO_ENABLED=1 go build -trimpath -o /templogger .

FROM debian:bookworm-slim
RUN useradd --system --uid 10001 --create-home templogger && mkdir /data && chown templogger:templogger /data
USER templogger
WORKDIR /data
COPY --from=build /templogger /usr/local/bin/templogger
EXPOSE 9000
ENTRYPOINT ["templogger"]
CMD ["--web", "--bind", "0.0.0.0", "--db", "/data/readings.sqlite", "--file", "/data/readings.csv"]
