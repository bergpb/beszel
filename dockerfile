FROM --platform=$BUILDPLATFORM golang:alpine as builder

WORKDIR /app

# Download Go modules
COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
COPY migrations ./migrations

# Build
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-w -s" -o /server .

# ? -------------------------
FROM alpine:latest

RUN apk add --no-cache \
    unzip \
    ca-certificates

COPY --from=builder /beszel /

COPY ./site/dist /site/dist

EXPOSE 8080

ENTRYPOINT [ "/beszel" ]

CMD ["serve", "--http=0.0.0.0:8080"]