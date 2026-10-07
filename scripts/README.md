# RayApi 二开镜像与升级

这套流程只服务于 `luckkai1989/new-api` 的 `main` 分支，不改变 New API 官方的 DockerHub 发布工作流。
GitHub Actions 编译镜像，服务器只负责拉取、校验、备份和切换，不再运行 Bun/Go 构建。

## GitHub 一次性设置

1. 进入自己的仓库 `https://github.com/luckkai1989/new-api`，打开 **Actions**。
   如果 fork 的工作流被禁用，先启用；在 **Settings → Actions → General** 允许 GitHub 和 Docker 的 Actions。
2. 在 **Settings → Secrets and variables → Actions → Variables** 新建仓库变量：
   名称 `RAYAPI_IMAGE_BUILD_ENABLED`，值 `true`（小写）。开启后，每次推送 `main` 自动构建。
   未设置时不自动构建；仍可在 Actions 的 **RayApi fork image (GHCR)** 页面手动点击 **Run workflow**，分支选 `main`。
3. 工作流自动使用 `GITHUB_TOKEN`，只申请 `contents: read` 和 `packages: write`。
   不需要新增 DockerHub 密钥，也不需要重新登录 GitHub；组织策略限制 Actions/Packages 时需管理员放行。
4. 首次成功后，镜像出现在个人账号的 **Packages → new-api**。
   新包默认私有，本流程不改变可见性。已有同名包时，检查 **Package settings → Manage Actions access**，允许此仓库写入。
5. 等对应提交的 Actions 成功后再升级。摘要会显示完整提交号、镜像标签和摘要。
   标签为 `ghcr.io/luckkai1989/new-api:sha-完整40位提交号`，只构建 `linux/amd64`。

构建工作流不需要重新授权登录。但推送这次新增的 `.github/workflows/` 文件时，如果 Git 提示缺少 `workflow` 权限，
需要为当前 Git 凭据补充该权限，或者使用已有适当权限的 SSH/令牌；不要把生产服务器的只读 GHCR PAT 扩大成推送凭据。
参考：[GitHub workflow scope](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/scopes-for-oauth-apps)。

参考：[GitHub Container Registry](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)、
[Actions 权限设置](https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/enabling-features-for-your-repository/managing-github-actions-settings-for-a-repository)。

## 服务器私有镜像认证

私有包需要服务器所在用户登录 GHCR。为账号创建到期时间有限的 **Personal access token (classic)**，
只选 `read:packages`，账号须有这个包的读取权限；组织开启 SSO 时按要求授权。
不要把 PAT、NewAPI 密钥或密码放到脚本、Git、命令行参数或聊天中。

在服务器交互终端执行，提示 Password 时输入 PAT，不是 GitHub 登录密码：

```bash
docker login ghcr.io -u luckkai1989
```

Docker 默认可能将凭据保存在 root 的 Docker 配置中，建议使用 credential helper，并限制 root 配置访问权限。
若你明确选择公开镜像，需要在 GitHub 包设置中手动改为 public；公开包可匿名拉取。公开转换可能无法撤销，请先确认。
这里使用 `GITHUB_TOKEN` 发布镜像不等于自动授权生产服务器；生产服务器只需读权限，不需要写权限。

## 安装新版运维脚本

提交并推送本次代码后，在服务器执行下面的准备操作。这只更新源码和脚本，不升级容器：

```bash
git -C /root/newapi-update/new-api pull --ff-only origin main
cp -a /root/update-newapi.sh "/root/update-newapi.sh.before-ghcr-$(date +%Y%m%d-%H%M%S)"
install -m 700 /root/newapi-update/new-api/scripts/update-newapi.sh /root/update-newapi.sh
bash /root/update-newapi.sh --check
```

保留原来的 `/opt/newapi/docker-compose.yml`、`.env` 和 `docker-compose.custom.yml`。
`UPSTREAM_MONITOR_ENCRYPTION_KEY` 必须沿用当前的持久化密钥，至少 32 字节，并通过 Compose 传入容器，不能重新生成替换。
不要把脚本直接放到源码 checkout 内修改，否则选项 6 会因源码不干净而拒绝升级。

## 运维选项

运行 `bash /root/update-newapi.sh` 查看菜单，或使用对应参数。

| 选项 | 参数 | 操作 |
| --- | --- | --- |
| 1 | `--upgrade` | 拉取官方 `calciumion/new-api:latest`，完整备份后升级；二开镜像启用时拒绝执行 |
| 2 | `--backup` | 短暂停止 NewAPI，备份 MySQL、Redis、应用文件和配置，再启动原容器 |
| 3 | `--backup-db` | 在线一致性备份 MySQL，不停服；要求业务表均为 InnoDB |
| 4 | `--backup-images` | 选项 2 加导出三个容器当前镜像，占用更多磁盘 |
| 5 | `--check` | 只检查，不拉镜像、不停服、不修改部署 |
| 6 | `--upgrade-custom` | 快进拉取 fork 的 `main`，拉取对应 GHCR 镜像，完整备份后只替换 NewAPI |
| 0 | 无 | 退出 |

旧参数 `--upgrade-source` 是选项 6 的兼容别名，现在也只拉镜像，不会回退到服务器编译。

选项 6 会校验提交号、fork 来源、Linux amd64 平台和镜像摘要，然后以 `@sha256:...` 固定部署。
镜像未构建、无读取权限、数据校验失败、磁盘不足时保持原服务在线；没有自动用旧镜像凑数的逻辑。
镜像拉取前后均检查空间，取消原来额外的 5 GiB 构建预留，仍保留备份估算和 3 GiB 安全余量。

升级只增加/更新独立的 `docker-compose.image.yml`，该文件仅存镜像摘要。
原有 custom 文件中的加密密钥、环境变量等不会被覆盖；Compose 按 base、custom（若有）、image 顺序合并。
以后手动调用 Compose 也必须使用相同的 `-p` 项目名及全部 `-f` 文件，不要用单个 base 文件误切回官方镜像。

备份放在 `/opt/newapi-backups/时间-随机后缀`，包含配置、SQL、Redis RDB、应用压缩包、旧镜像标签和目标摘要。
成功后只保留最近三份脚本生成的完整备份，失败或手工备份不会删；只清理 dangling 镜像，带标签的回滚镜像保留。
不清空数据库、不删除卷、不重建 MySQL/Redis，不自动恢复 SQL；一旦新版本可能执行迁移，失败也不自动降级。
本流程不再产生构建缓存，因此不执行全局 `docker builder prune`。历史遗留缓存可在确认其他构建不需要后单独清理。
备份含凭据，应限制权限并另存一份到安全位置；镜像摘要校验不能代替端到端业务验证。

## 验证

在 Linux/WSL 执行以下测试，所有 Docker/Redis/数据库操作均为临时目录中的模拟命令，不触碰真实部署：

```bash
bash -n scripts/update-newapi.sh
python3 -B scripts/test_upgrade_newapi.py
```

生产升级后仍需人工验证登录、余额、上游监控和一个真实模型请求。
