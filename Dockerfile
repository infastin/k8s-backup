# Build
FROM golang:1.23-alpine3.21 AS build
RUN apk add git

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o build/ ./

# Run
FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build /app/build/k8s-backup /app/k8s-backup
ENTRYPOINT ["/app/k8s-backup"]
