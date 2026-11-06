FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /ink .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /ink /ink
ENV ADDR=:8080 TRUSTED_PROXY_HEADER=X-Forwarded-For
EXPOSE 8080
ENTRYPOINT ["/ink"]
