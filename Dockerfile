# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
RUN apk add --no-cache build-base sqlite-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
ENV CGO_ENABLED=1
RUN go build -o /out/bridge ./...

FROM alpine:3.20
RUN apk add --no-cache ca-certificates sqlite-libs
RUN adduser -D -u 10001 bridge
USER bridge
WORKDIR /app
COPY --from=build /out/bridge /app/bridge
ENV STORE_DIR=/data
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/app/bridge"]
