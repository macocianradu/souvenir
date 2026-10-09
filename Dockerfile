FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go vet ./... && go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/souvenir .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -H -u 10001 souvenir \
    && mkdir -p /data && chown souvenir /data
WORKDIR /app
COPY --from=build /out/souvenir /usr/local/bin/souvenir
COPY deploy/config.json /app/.config.json
COPY deploy/entrypoint.sh /usr/local/bin/entrypoint.sh
USER souvenir
VOLUME /data
EXPOSE 23234
ENV SOUV__ssh__hostKeyPath=/data/ssh_host_ed25519 \
    SOUV__ssh__authorizedKeys=/data/authorized_keys
ENTRYPOINT ["entrypoint.sh"]
CMD ["serve"]
