# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS backend-build
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /harper .

FROM alpine:3.23 AS backend
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 harper
WORKDIR /app
COPY --from=backend-build /harper /app/harper
USER harper
EXPOSE 4004
HEALTHCHECK --interval=10s --timeout=4s --start-period=5s --retries=3 CMD ["/app/harper", "-healthcheck"]
ENTRYPOINT ["/app/harper"]

FROM node:24-alpine AS frontend-build
WORKDIR /src
COPY frontend/package*.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM nginx:1.28-alpine AS frontend
COPY frontend/nginx.conf /etc/nginx/nginx.conf
COPY --from=frontend-build /src/dist /usr/share/nginx/html
USER nginx
EXPOSE 4003
HEALTHCHECK --interval=10s --timeout=4s --start-period=5s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:4003/healthz || exit 1
ENTRYPOINT ["nginx"]
CMD ["-g", "daemon off;"]
