FROM oven/bun:1.3.14-alpine AS build
WORKDIR /app
COPY package.json bun.lock ./
# 生产镜像必须使用锁文件安装，避免构建时悄悄解析出与仓库不一致的依赖版本。
RUN bun install --frozen-lockfile
COPY . .
RUN bun run build

FROM nginx:1.27-alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 80
STOPSIGNAL SIGQUIT
CMD ["nginx", "-g", "daemon off;"]
