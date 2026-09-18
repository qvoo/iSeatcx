# iSeat · 超星图书馆座位自动预约系统

> 超星（学习通）自习室座位自动预约系统：自动抢座、一键签到、续约到闭馆。
> 支持 **多账号**，每个账号可绑定 **不同的学校**；Web 管理系统 + Docker 一键部署。

[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

---

## ✨ 功能特性

| 功能 | 说明 |
| --- | --- |
| 🎯 自动抢座 | 到点自动高频重试，签名参数 + 行为验证自动完成 |
| 📝 自动签到 | 预约生效窗口自动签到，无需现场扫码 |
| 🔁 自动续约 | 每段自动衔接，自动适配每个自习室真实的开放/闭馆时间 |
| 📅 跨天循环 | 今日占座到闭馆后自动预约明日，天天循环（任务引擎无人值守） |
| 🕖 各校放号规则 | 按账号设置**抢座时刻**与**放号方式**：前一天放号（如 19:00 抢明天）/ 当天早上放号（如 07:00 抢当天） |
| 🌗 一次约满整天 | 有些学校可一次预约到闭馆：自动找闭馆时间，一段铺满整天，不用分段 |
| 🏫 自习室列表 | 自动拉取全部自习室及其开放/闭馆时间 |
| 🪑 座位网格 | 亮=可选、暗=占用的可视化方块，点选代替手输 |
| 👥 多账号管理 | 可添加多个超星账号；**每个账号可配置自己的学校** |
| 🔎 链接自动识别 | 粘贴预约大厅链接即自动识别该系统代际/学校参数/放号规则 |
| ⚡ 快速预约 | 预约过的桌子一键续约（今日/明日/每天） |
| 🗂 任务管理 | 暂停/恢复/删除任务，手动取消/退座；显示学校+房间 |
| 🌐 Web 管理 | Vue3 + Go 管理系统，Docker 一键部署 |

## 🧱 技术栈

- **后端**：Go + Gin（REST API + 静态托管 + 任务调度器）
- **前端**：Vue 3 + TypeScript + Vite
- **数据库**：MySQL（生产） / SQLite（本地兜底）
- **部署**：Docker / docker-compose

```
server/            Go 后端
  ├─ cxclient.go   超星接口封装（登录/抢座/签到/退座/房间/闭馆遍历）
  ├─ captcha.go    行为验证自动求解（按学校 captchaId）
  ├─ scheduler.go  任务引擎：抢座→签到→续约→跨天循环
  ├─ secret.go     密钥管理与密码加密
  └─ handlers.go   REST API
web/               Vue3+TS 前端
  ├─ Login.vue     超星账号登录
  └─ Dashboard.vue 手动选座 + 任务管理 + 快速预约 + 账号管理
schema.sql         MySQL 建库脚本
Dockerfile         多阶段构建镜像
docker-compose.yml MySQL + Web 一键部署
```

## 🚀 快速开始

### 方式一：Docker Compose（推荐）
```bash
git clone https://github.com/qvoo/iSeatcx.git && cd iSeatcx
# 编辑 docker-compose.yml 修改数据库密码
docker compose up -d --build
# 打开 http://localhost:5251
```

### 方式二：本地运行
```bash
cd server && go build -o seatbook.exe . && ./seatbook.exe   # http://localhost:5251
cd web && npm install && npm run build                       # 前端产物由 Go 自动托管
```

## 🖥 Web 系统三大功能

1. **手动选择其他座位**
   - 作用域：`单账号` 或 `全部账号·批量`（批量给每个账号分配不同座位）
   - 自习室下拉（自动含闭馆时间）→ 今天/明天切换 → 座位方块网格（亮=可选）→ 确认预约
2. **任务管理**
   - 展示所有账号的占座任务及其所在**学校 + 房间 + 座位**；暂停/恢复/删除任务
   - 当前预约：取消 / 退座（自动带对应账号会话）
3. **快速预约**
   - 展示各个账号预约过的桌子 → 一键续约今日/明日/每天（按归属账号预约）

## 👥 账号管理与学校参数（重点）

系统支持**多账号**，且**每个账号可以属于不同的学校**。在页面右上角「管理账号」浮层中，可以为每个账号维护它自己的学校参数。

为什么每个账号要单独配学校？因为超星座位系统的座位业务 `seatId`、单位 `deptIdEnc`、座位 `seatIdEnc`、滑块验证码 `captchaId` 都**因学校而异**。同一套默认值只适用于一所学校，所以不同学校的账号必须各自绑定。

### 参数含义

| 参数 | 说明 | 默认 |
| --- | --- | --- |
| `seat_id` | 该校的**座位业务 ID**（对应座位页 `seatId`），用于查房间、选座、预约、签到 | `105` |
| `dept_id_enc` | 该校/单位的 **deptIdEnc**（自习室列表用） | `0fd2b43990df8985` |
| `seat_id_enc` | 该校的**座位业务 seatIdEnc**（我的预约查询用） | `9dffbb2440d6a600` |
| `captcha_id` | 该校**滑块验证码 captchaId** | `42sxgHoTPTKbt0uZxPJ7ssOvtXr3ZgZ1` |

> 这些值可在浏览器打开对应学校的超星座位页，从页面源码 / 接口请求里找到。

### 常见学校对照（示例）

| 学校 | seat_id | dept_id_enc | seat_id_enc | captcha_id |
| --- | --- | --- | --- | --- |
| **海南大学** | `105` | `0fd2b43990df8985` | `9dffbb2440d6a600` | `42sxgHoTPTKbt0uZxPJ7ssOvtXr3ZgZ1` |

> 上面的 `105 = 海南大学` 是内置默认值（`CX_SEAT_ID`）。如果你要添加其他学校账号，请在「管理账号」里把对应参数改成那所学校的值。留空则自动使用默认值（海南大学）。

### 使用提示

- **添加账号**：在「管理账号」里填 `手机号 / 密码`，学校参数选填（留空用默认）；每个账号可绑定自己的学校。
- **修改学校**：点击账号右侧的「学校」按钮展开参数编辑，改完点「保存学校」。改完后该账号的客户端会话会失效并自动重新登录。
- **作用域为「全部账号·批量」时**：批量只对**与所选房间同校**的账号生效；跨校账号请先切换到该校房间再单独批量。
- **单账号**：完全按该账号自己的学校查房间、选座、预约、签到。

## 📦 获取 Docker 镜像

### 方式 A：容器仓库直接拉取（推荐）
```bash
docker pull ghcr.io/qvoo/iseatcx:latest
# 或指定版本
docker pull ghcr.io/qvoo/iseatcx:v1.1.3

# 运行
docker run -d --name iseat -p 5251:5251 -v iseat_data:/data ghcr.io/qvoo/iseatcx:latest
```

### 方式 B：下载离线镜像包
从本仓库 **Releases** 下载 `iseat.tar`，然后：
```bash
docker load -i iseat.tar
docker run -d --name iseat -p 5251:5251 -v iseat_data:/data iseat:latest
```

> 提示：两种方式等价；离线 tar 适合内网/无外网环境。

## ⚙️ 配置（环境变量）

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `PORT` | 5251 | 服务端口 |
| `MYSQL_DSN` | 空 | MySQL 连接串；空则 SQLite |
| `SQLITE_PATH` | seatbook.db | SQLite 文件路径 |
| `SEAT_SECRET` | 自动生成 | 本地存储加密密钥（建议生产显式配置） |
| `CX_BASE` / `CX_LOGIN_URL` | office.chaoxing.com / passport2 fanyalogin | 超星对接地址 |
| `CX_SEAT_ID` | 105 | **默认学校**的座位业务 ID（海南大学） |
| `CX_DEPT_ENC` | 0fd2b43990df8985 | 默认学校 deptIdEnc |
| `CX_SEAT_ENC` | 9dffbb2440d6a600 | 默认学校 seatIdEnc |
| `CX_CAPTCHA_ID` | 42sxgHoTPTKbt0uZxPJ7ssOvtXr3ZgZ1 | 默认学校滑块验证码 captchaId |
| `WEB_DIR` | ../web/dist | 前端静态目录 |
| `TZ` | Asia/Shanghai | 时区 |

> `CX_SEAT_ID` / `CX_DEPT_ENC` / `CX_SEAT_ENC` / `CX_CAPTCHA_ID` 是**默认学校**（海南大学）的兜底参数；新账号未单独配置时使用这些默认值，已在「账号管理」里配置了学校的账号按各自参数走。

## 🛡 安全设计

- **密码加密存储**：账号密码使用 AES-256-GCM 加密后入库
- **密钥独立**：加密密钥从环境变量或独立密钥文件读取，不随仓库分发
- **会话自愈**：超星登录失效自动重登，长期无人值守
- **数据持久化**：数据库/密钥随数据卷持久化，容器重启不丢失
- 生产部署建议启用 HTTPS（反向代理 / Let's Encrypt）保护传输安全

## 🗄 数据库

表：`users`（登录用户，含学校参数）、`session_tokens`（登录会话）、`tasks`（占座任务）。`schema.sql` 建库，GORM 自动迁移。

## 📮 联系我们

- GitHub：[https://github.com/qvoo/iSeatcx](https://github.com/qvoo/iSeatcx)
- 问题反馈：欢迎在仓库提交 **Issue**

## ⚠️ 免责声明

本项目为开源学习工具，仅供学习交流。请使用者遵守学校座位预约规则与相关法律法规，因使用本工具产生的一切后果由使用者自行承担。

## 📄 License

MIT
