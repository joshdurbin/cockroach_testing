FROM golang:alpine AS builder
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /cdbct .

FROM alpine:latest
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /cdbct /usr/local/bin/cdbct
ENTRYPOINT ["cdbct"]
