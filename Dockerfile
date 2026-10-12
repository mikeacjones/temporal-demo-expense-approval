# One image, two processes: /app/web (UI + API, the default) and /app/worker.
# Run the worker by overriding the command: `docker run <image> /app/worker`,
# or `command: ["/app/worker"]` in deploy/expense-approval.yaml.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/web ./cmd/web \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /app/
EXPOSE 8080
CMD ["/app/web"]
