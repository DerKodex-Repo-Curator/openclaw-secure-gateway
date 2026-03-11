FROM golang:1.23-alpine AS build
WORKDIR /src
COPY gpg-approval-bot/main.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/gpg-approval-bot main.go

FROM alpine:3.20
RUN apk add --no-cache gnupg
RUN adduser -D -u 1000 bot
WORKDIR /app
COPY --from=build /out/gpg-approval-bot /app/gpg-approval-bot
COPY --chown=bot:bot gpg-approval-bot/scripts/entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh
USER bot
ENTRYPOINT ["/app/entrypoint.sh"]
