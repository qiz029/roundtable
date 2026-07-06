FROM golang:1.26.4-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/roundtabled ./cmd/roundtabled
RUN CGO_ENABLED=0 go build -trimpath -o /out/roundtable-agent ./cmd/roundtable-agent

FROM debian:bookworm-slim

RUN apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates curl \
	&& rm -rf /var/lib/apt/lists/*

RUN mkdir -p /app/data/avatars \
	&& useradd --uid 10001 --home-dir /app --shell /usr/sbin/nologin roundtable \
	&& chown -R roundtable:roundtable /app

COPY --from=build /out/roundtabled /usr/local/bin/roundtabled
COPY --from=build /out/roundtable-agent /usr/local/bin/roundtable-agent
COPY docker-entrypoint.sh /usr/local/bin/roundtabled-entrypoint.sh

WORKDIR /app
EXPOSE 8080

ENTRYPOINT ["sh", "/usr/local/bin/roundtabled-entrypoint.sh"]
CMD ["roundtabled", "--addr", ":8080"]
