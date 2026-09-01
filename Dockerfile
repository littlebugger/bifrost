FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /bifrost ./cmd/bifrost

FROM alpine:3.22
COPY --from=build /bifrost /usr/local/bin/bifrost
ENTRYPOINT ["bifrost"]
CMD ["-f", "/etc/bifrost/bifrost.hcl"]
