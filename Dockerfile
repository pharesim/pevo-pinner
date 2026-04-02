FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY . .
RUN go build -o /pevo-pinner .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=builder /pevo-pinner /usr/local/bin/
EXPOSE 8421 8080
ENTRYPOINT ["pevo-pinner"]
