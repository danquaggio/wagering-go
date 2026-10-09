FROM golang:1.26.8-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -o /out/wager ./cmd/wager
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/wager /app/wager
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/wager"]
