#!/usr/bin/env bash
# wb-login.sh — Windows 下添加/刷新 WorkBuddy 账号（原生模式：本地 login.exe，无需 Docker）
# 用法:  ./wb-login.sh
# 流程:  ./login.exe url → 浏览器登录 → ./login.exe poll → 落盘 auths/ → 签到 → 提示重启服务
set -euo pipefail
cd "$(dirname "$0")"

AUTH_DIR="./auths"
API_KEY=$(python -c "import json; print(json.load(open('config.json', encoding='utf-8')).get('api_key',''))" 2>/dev/null || echo "")
mkdir -p "$AUTH_DIR"

# 本目录不含源码；login.exe 缺失时需回源码目录构建后拷贝过来
LOGIN_EXE="./login.exe"
if [[ ! -x "$LOGIN_EXE" ]]; then
    echo "login.exe 缺失。请在源码目录执行:"
    echo "  go build -trimpath -ldflags=\"-s -w\" -o login.exe ./cmd/login"
    echo "并把生成的 login.exe 拷贝到本目录。"
    exit 1
fi

AUTH_URL=$("$LOGIN_EXE" url)
echo "请在浏览器打开以下链接完成登录（已尝试自动打开）："
echo "  $AUTH_URL"
python -c "import webbrowser; webbrowser.open('$AUTH_URL')" 2>/dev/null || true

echo ""
read -rp "完成登录后按 y 继续: " ans
if [[ "$ans" != "y" && "$ans" != "Y" ]]; then
    echo "已取消"; exit 1
fi

RESULT=$("$LOGIN_EXE" poll) || { echo "登录未完成或已超时，请重新运行本脚本"; exit 1; }
echo "$RESULT" > "$AUTH_DIR/.last_login.json"

# 解析并落盘 auth 文件（格式与 internal/auth 读取一致）
python - "$RESULT" <<'PYEOF'
import json, sys, time, os
r = json.loads(sys.argv[1])
uid = r.get("uid", "")
if not uid:
    print("无法获取 uid，token 可能无效"); sys.exit(1)
auth = {
    "account": {"uid": uid, "enterpriseId": r.get("enterprise_id", ""), "nickname": r.get("nickname", "")},
    "auth": {"accessToken": r["access_token"], "refreshToken": r["refresh_token"],
             "expiresAt": int(time.time()) + int(r["expires_in"]), "domain": r.get("domain", "")}
}
path = os.path.join("auths", f"workbuddy-{uid}.json")
json.dump(auth, open(path, "w", encoding="utf-8"), indent=1, ensure_ascii=False)
print(f"已保存: {path}  (nickname={r.get('nickname','?')})")
PYEOF

# 首次签到（幂等，失败不影响）
TOKEN=$(python -c "import json; print(json.load(open('$AUTH_DIR/.last_login.json'))['access_token'])")
WB_UID=$(python -c "import json; print(json.load(open('$AUTH_DIR/.last_login.json'))['uid'])")  # UID 是 bash 只读内建变量，不能赋值
curl -s -X POST "https://www.codebuddy.cn/v2/billing/meter/daily-checkin" \
  -H "Authorization: Bearer $TOKEN" -H "X-User-Id: $WB_UID" \
  -H "Content-Type: application/json" -H "Accept: application/json" \
  --data "{}" --connect-timeout 10 -m 15 | head -c 200 || true
echo ""

# 号池只在启动时读取 auths/ 目录，新号需要重启服务生效
echo ""
echo "凭证已保存。请重启 wb2api.exe 使新账号生效：关掉服务窗口，重新双击 start.bat。"
