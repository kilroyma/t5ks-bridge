FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /t5ks-bridge .

FROM scratch
COPY --from=build /t5ks-bridge /t5ks-bridge
ENTRYPOINT ["/t5ks-bridge"]
