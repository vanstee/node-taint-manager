FROM golang:latest as builder

WORKDIR /build

COPY go.mod go.sum .
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o ./node-taint-manager

FROM scratch
WORKDIR /app
COPY --from=builder /build/node-taint-manager ./node-taint-manager
ENTRYPOINT ["./node-taint-manager"]
