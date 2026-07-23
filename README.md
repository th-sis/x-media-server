# X-Media Server

> Go gRPC 后端服务 — Docker Compose 部署在 NAS 上

## 快速部署

```bash
docker compose -f truenas-compose.yml up -d
```

## 访问

- 管理面板：http://192.168.7.154:35678/config
- 健康检查：http://192.168.7.154:35678/healthz
- gRPC：192.168.7.154:50051

## 技术栈

- Go 1.22 + SQLite (WAL)
- gRPC + HTTP 双端口
- Docker 多阶段构建 < 30MB
