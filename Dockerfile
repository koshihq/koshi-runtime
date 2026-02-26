FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /koshi ./cmd/koshi

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /koshi /koshi

USER nonroot:nonroot

ENTRYPOINT ["/koshi"]
