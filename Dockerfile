FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/datadocket ./cmd/datadocket

FROM alpine:3.22
RUN addgroup -S app && adduser -S -G app app && mkdir -p /data/uploads && chown -R app:app /data
COPY --from=build /out/datadocket /usr/local/bin/datadocket
USER app
ENV UPLOAD_DIR=/data/uploads
EXPOSE 8080
ENTRYPOINT ["datadocket"]
