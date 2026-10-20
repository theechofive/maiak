# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM node:22-alpine AS build
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run build

FROM nginxinc/nginx-unprivileged:1.29-alpine
COPY nginx.conf /etc/nginx/nginx.conf
COPY security-headers.conf /etc/nginx/security-headers.conf
COPY --from=build /src/dist /usr/share/nginx/html
EXPOSE 8080
USER 101
