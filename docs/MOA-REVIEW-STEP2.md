# MoA Step 2 自审文档：Go 后端全量交付 + CI/CD + 测试策略

> 审查范围：x-media-server 全部代码 + 部署 + 测试方案
> 日期：2026-07-23

---

## 1. 已完成事项

| 模块 | 状态 | 说明 |
|------|------|------|
| .proto 契约 | ✅ | api.proto，5服务17 RPC，go:embed 编译 |
| Go 后端骨架 | ✅ | 14文件，gRPC + HTTP 双端口 |
| gRPC stub 生成 | ✅ | api.pb.go (155KB) + api_grpc.pb.go (55KB) |
| 编译通过 | ✅ | Windows + Docker 双平台 |
| GitHub 仓库 | ✅ | github.com/th-sis/x-media-server |
| CI/CD | ✅ | GitHub Actions → Docker Hub 自动构建 |
| Docker 镜像 | ✅ | thsis/x-media-server:latest (11.4 MB) |
| TrueNAS YAML | ✅ | 已部署在 192.168.7.154:35678 |
| 11页管理面板 | ✅ | go:embed 注入，居中 + 放大字体 |

## 2. 待测试验证

| 测试项 | 状态 | 方法 |
|--------|------|------|
| Auth Login | ⏳ | gRPC 调用 |
| ControlStream 双向流 | ⏳ | 必须触发转存状态机 |
| 115 转存状态机 | ⏳ | 异步双阶段 |
| 盘搜聚合 | ⏳ | 多源并发 |
| 健康检查 | ⏳ | HTTP /healthz |

## 3. 架构合规性

- ✅ state.go (sync.Map) + lru.go (16分片) 无全局锁
- ✅ go:embed 单二进制，无文件路径依赖
- ✅ 异步双阶段转存（STATUS_TRANSFERRING）
- ✅ gRPC 拦截器（Login + HealthCheck 豁免）
- ✅ Docker < 30MB

## 4. 审查问题

1. gRPC 双向流 ControlStream 是否正确实现了心跳（Ping/Pong）？
2. 转存状态机 TranferService.StartTransfer 在 ControlStream 完成后如何推送结果？
3. 盘搜聚合 SearchEngine 接口是否支持并发超时（2秒截止）？
4. admin.html 中的 API 路径是否与 handlers.go 中注册的路由完全匹配？
5. SQLite 数据库迁移中 transfer_tasks 表的状态字段是否与 model.TransferStatus 常量一致？
