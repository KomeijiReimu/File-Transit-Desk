FROM oven/bun:1.3.14-debian AS deps
WORKDIR /app
COPY package.json bun.lock ./
# Bun 只负责按锁文件安装依赖。
RUN bun install --frozen-lockfile

FROM node:22-trixie-slim AS build
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
# vue-tsc 和 Vite 使用真实 Node，避免 Bun runner 的 Vue SFC 解析兼容问题。
RUN node -e "if (process.versions.bun) throw new Error('Build requires real Node.js')" \
    && node ./node_modules/vue-tsc/bin/vue-tsc.js -b \
    && node ./node_modules/vite/bin/vite.js build

FROM nginx:1.27-alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 80
STOPSIGNAL SIGQUIT
CMD ["nginx", "-g", "daemon off;"]
