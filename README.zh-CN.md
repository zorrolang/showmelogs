# showmelogs（smlog）

[English version: README.md](README.md)

轻量化测试结果归档与检索服务（Go 版）—— 为在 Linux 测试机上运行
`xfstests` / `LTP` / `fio` / `blktests` 等测试的个人场景设计：

- **低内存占用**：典型运行内存远低于 20MB（流式 ZIP、无目录缓存、前端虚拟滚动、
  空闲自动归还内存）
- **零第三方依赖**：仅使用 Go 标准库，单二进制内嵌前端页面，拷到测试机即可运行
- **局域网跨主机访问**：监听 `0.0.0.0`，浏览器打开 `http://<测试机IP>:8080` 即可用

## 功能一览

| 功能 | 说明 |
| --- | --- |
| 通用文件浏览 | 任意层级目录浏览、面包屑导航、按名称/大小/时间排序 |
| 标签徽章 | 自动识别 `FAIL` / `PASS` / `xfstests` / `LTP` / `fio` / `blktests`（按目录/文件名关键词，忽略大小写） |
| 模式 A：目录/文件名检索 | 内存并发遍历目录树，多关键词空格分隔（如 `6.12 ext4`），毫秒级返回，上限 200 条 |
| 模式 B：文件内容检索 | 并发扫描纯文本日志（如 `Call Trace`、`BUG:`、`FAILED`），返回文件路径 + 行号 + 文本片段；自动跳过隐藏文件、二进制文件与 >20MB 的大文件；每文件最多 5 条匹配，总文件数上限 100 |
| 分屏在线预览 | 点击文本文件在右侧分屏高亮预览；>512KB 自动截取「前 128KB + 末尾 384KB」，可一键加载完整内容（上限 10MB）；二进制文件提示直接下载 |
| 打包下载 | 单文件原样下载（HTTP Range 断点续传）；目录点击「打包下载」后端 `archive/zip` 流式实时打包，不产生临时磁盘文件 |
| 安全 | 全程只读（无删除/修改接口）、路径穿越拦截（`../` 一律 403）、软链接越权拦截、可选 HTTP Basic Auth |

## 快速开始

```bash
# 构建（需要 Go 1.21+，仅标准库）
make build            # 或 go build -o smlog .

# 交叉编译（测试机常见架构）
make build-linux-amd64
make build-linux-arm64

# 准备结果目录并启动
mkdir -p /data/test_results
./smlog -root /data/test_results -addr 0.0.0.0:8080
```

浏览器访问 `http://<本机IP>:8080`。

> 默认根目录为 `/data/test_results`，不存在时启动会给出清晰提示，
> 可用 `-root` 或环境变量 `SMLOG_ROOT` 指向任意目录。

## 命令行参数与环境变量

所有参数都支持读取同名环境变量（前缀 `SMLOG_`，大写）作为默认值，
命令行参数优先。

| 参数 | 环境变量 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `-root` | `SMLOG_ROOT` | `/data/test_results` | 测试结果根目录 |
| `-addr` | `SMLOG_ADDR` | `0.0.0.0:8080` | 监听地址与端口 |
| `-user` | `SMLOG_USER` | 空 | Basic Auth 用户名（设置 `-pass` 时默认 `admin`） |
| `-pass` | `SMLOG_PASS` | 空 | Basic Auth 密码；**非空即开启全站鉴权**，为空则局域网免密访问 |
| `-dev` | `SMLOG_DEV` | `false` | 开发模式：每次请求从磁盘读取 `web/index.html`，便于实时调优 UI（需在项目根目录运行） |
| `-follow-symlinks` | `SMLOG_FOLLOW_SYMLINKS` | `true` | 是否解析根目录内的软链接；解析后目标真实路径必须仍在根目录内，否则 403 |

示例：

```bash
# 局域网免密
./smlog -root /data/test_results

# 开启 Basic Auth
./smlog -root /data/test_results -user admin -pass 's3cret!'

# 通过环境变量配置
SMLOG_ROOT=/data/test_results SMLOG_ADDR=0.0.0.0:8080 \
SMLOG_PASS='s3cret!' ./smlog
```

## 目录组织与命名建议

建议将根目录固定为 `/data/test_results`，按「日期_套件_版本_文件系统」命名
每个测试输出目录，例如：

```text
/data/test_results/
├── 20261117_xfstests_6.12_xfs/      # 当天 xfstests 在 6.12 + xfs 上的结果
├── 20261118_xfstests_6.12_ext4/
├── 20261119_ltp_6.12/
├── 20261120_fio_nvme/
└── 20261121_blktests_sda/
```

命名中包含 `fail` / `.bad` / `error` 会标红 `FAIL`，包含 `pass` 标绿 `PASS`，
包含 `xfstests` / `ltp` / `fio` / `blktests` 自动显示对应徽章。

### 用软链接整合不同位置的测试输出

`xfstests` 的 `results/` 与 `LTP` 的 `output/` 往往不在同一目录，
可以直接在根目录里建软链接（默认开启 `-follow-symlinks`）：

```bash
ln -s /root/xfstests/results/20261117_run /data/test_results/20261117_xfstests_6.12_xfs
ln -s /opt/ltp/output/20261119_run      /data/test_results/20261119_ltp_6.12
```

安全策略：

- 软链接**目标解析后必须仍位于根目录内**，指向 `/etc` 等外部位置的链接
  不会出现在列表里，直接访问返回 `403`；
- 软链接环路（`a/link -> a`）由 visited 集合防护，搜索不会死循环；
- 关闭 `-follow-symlinks=false` 时，列表与搜索隐藏全部软链接，访问返回 `403`。

## 防火墙放行端口（跨主机访问）

```bash
# firewalld（RHEL/Rocky/CentOS/Fedora）
sudo firewall-cmd --permanent --add-port=8080/tcp
sudo firewall-cmd --reload

# ufw（Debian/Ubuntu）
sudo ufw allow 8080/tcp
```

> 提示：HTTP Basic Auth 为明文传输（未加密），仅建议在可信内网使用；
> 如需公网访问请用 SSH 隧道或反向代理加 TLS。

## Systemd 常驻部署

```bash
sudo make install                        # 安装到 /usr/local/bin/smlog
sudo cp smlog.service /etc/systemd/system/
# 编辑服务文件：修改 User= 与 ExecStart= 中的目录、端口、账号密码
sudo systemctl daemon-reload
sudo systemctl enable --now smlog
sudo systemctl status smlog
```

服务模板自带只读加固（`ProtectSystem=strict` 等），与本服务的只读设计一致。

## API 一览

所有接口仅接受 `GET`；错误统一返回 `{"error": "..."}` 及 `400/401/403/404/500`。

| 接口 | 说明 |
| --- | --- |
| `GET /` | 内嵌前端单页 |
| `GET /api/list?path=<rel>` | 目录列表（目录优先，mtime 倒序），返回 `current_path` / `parent_path` / `items[]`（含自动标签 `tags`） |
| `GET /api/search?q=<kw>&mode=name\|content&scope=<rel>` | 双模式检索；`q` 空格分隔多关键词（需全部匹配、忽略大小写）；`scope` 为空搜全根目录 |
| `GET /api/preview?path=<rel>&full=<bool>` | 文本预览；`is_binary` 标记二进制；>512KB 截断返回并带 `truncated`；`full=1` 完整加载（上限 10MB） |
| `GET /api/download?path=<rel>` | 文件：原样下载（支持 Range）；目录：流式 ZIP 打包 |

## 开发与调优前端

```bash
# -dev 模式下每次刷新都重新读取磁盘上的 web/index.html，
# 改完 CSS/JS 直接刷新浏览器即可（须在项目根目录运行）
./smlog -root ./test_results -addr 127.0.0.1:8080 -dev
```

前端零外部依赖（无 CDN），离线内网可直接使用。

## 测试

```bash
make test          # go test -v ./...
```

覆盖：路径穿越拦截（`../../etc/passwd` → 403）、目录排序与标签识别、
双模式搜索（多关键词 / 行号提取 / 跳过二进制、隐藏、超大文件）、
大文件截断预览、流式 ZIP 打包解压校验、Basic Auth 开关鉴权、
软链接越权与环路防护。

## 资源占用说明

- 目录列表与搜索均为**按需实时遍历**，不做内存缓存；
- ZIP 打包为**流式输出**，不落临时文件；
- 前端阅读器采用**虚拟滚动**，10MB 日志也只渲染可视区域；
- 实际存活堆通常只有几 MB（GC 日志可见）；服务内置**空闲内存回收器**
  （空闲超过 2 分钟后每 5 分钟触发一次 `FreeOSMemory`），配合
  `GODEBUG=madvdontneed=1`（systemd 模板已默认设置）时，稳态 RSS
  长期保持在 20MB 以下、贴近真实占用。
  > 注：不设该环境变量时 Go 默认使用 `MADV_FREE`，已释放页在系统空闲时
  > 仍计入 RSS（数字虚高但内核随时可回收，不影响其他进程）。
