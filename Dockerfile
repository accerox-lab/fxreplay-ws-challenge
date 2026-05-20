FROM golang:1.22-alpine AS build
WORKDIR /src
COPY app/go.mod app/go.sum ./
RUN go mod download
COPY app/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/ws-server .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ws-server /ws-server
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/ws-server"]
