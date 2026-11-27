FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /app .

# Includes CA certificates for HTTPS (prize API) and SMTP over TLS.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /app /app
ENTRYPOINT ["/app"]
