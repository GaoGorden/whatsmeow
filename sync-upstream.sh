#!/bin/bash
# whatsmeow 上游同步脚本
# 用途：自动化合并 tulir/whatsmeow 上游更新
# 使用方式：在 whatsmeow 根目录运行 ./sync-upstream.sh

set -e

echo "=== whatsmeow 上游同步脚本 ==="
echo ""

# 检查是否在 whatsmeow 根目录
if [ ! -f "go.mod" ] || ! grep -q "go.mau.fi/whatsmeow" go.mod; then
    echo "❌ 错误：请在 whatsmeow 根目录运行此脚本"
    exit 1
fi

# 检查是否有未提交的更改
if ! git diff --quiet HEAD; then
    echo "❌ 错误：工作区有未提交的更改，请先提交或暂存"
    exit 1
fi

# 配置 Go 环境
GOSDK_DIR="D:/developmentKit/gosdk"
TOOLCHAIN_VER=$(grep "toolchain" go.mod | awk '{print $2}' 2>/dev/null || echo "go1.25.0")
GOSDK_VER=$(ls "$GOSDK_DIR" 2>/dev/null | sort -V | grep -E "^go" | tail -1)

if [ -z "$GOSDK_VER" ]; then
    echo "⚠️ 未找到 Go SDK，尝试使用系统默认 Go"
else
    export GOROOT="$GOSDK_DIR/$GOSDK_VER"
    export PATH="$GOROOT/bin:$PATH"
    export GOTOOLCHAIN=local
    echo "✓ 使用 Go SDK: $GOSDK_VER"
fi

echo "✓ 环境检查通过"
echo ""

# 步骤 1：拉取上游
echo "步骤 1/5：拉取上游代码..."
git fetch upstream
echo "✓ 上游代码已更新"
echo ""

# 步骤 2：创建同步分支
SYNC_BRANCH="sync-upstream-$(date +%Y%m%d)"
echo "步骤 2/5：创建同步分支 $SYNC_BRANCH..."
git checkout -b "$SYNC_BRANCH" upstream/main
echo "✓ 同步分支已创建"
echo ""

# 步骤 3：合并主分支
echo "步骤 3/5：合并主分支的定制代码..."
if git merge main -m "sync: merge custom code from main into sync branch"; then
    echo "✓ 主分支合并成功（无冲突）"
else
    echo "⚠️ 合并时出现冲突，需要手动解决"
    echo ""
    echo "=== 冲突处理指南 ==="
    echo ""
    echo "以下文件可能需要手动处理："
    echo ""
    echo "1. client.go"
    echo "   - 接受上游改动：git checkout --theirs client.go"
    echo "   - 然后手动添加 OnLoginSuccess 回调（在 PrePairCallback 之后）"
    echo ""
    echo "2. connectionevents.go"
    echo "   - 接受上游改动：git checkout --theirs connectionevents.go"
    echo "   - 然后手动添加 OnLoginSuccess() 调用（在 isLoggedIn.Store(true) 之后）"
    echo ""
    echo "3. pair.go"
    echo "   - 接受上游改动：git checkout --theirs pair.go"
    echo "   - 然后在 getQRClientType() 的 switch 中添加 IOS_PHONE case"
    echo ""
    echo "以下文件与上游完全一致，无需处理："
    echo "- store/clientpayload.go ✓"
    echo "- types/events/events.go ✓"
    echo ""
    echo "解决冲突后执行："
    echo "  git add ."
    echo "  git commit -m 'sync: resolve merge conflicts'"
    echo ""
    exit 1
fi

# 步骤 4：验证编译
echo ""
echo "步骤 4/5：验证编译..."
if go build ./...; then
    echo "✓ 编译通过"
else
    echo "❌ 编译失败，请检查代码"
    echo "提示：可能需要手动指定 Go SDK 路径"
    echo "  export GOROOT=D:/developmentKit/gosdk/go1.xx.x"
    echo "  export GOTOOLCHAIN=local"
    exit 1
fi

# 步骤 5：合并回主分支
echo ""
echo "步骤 5/5：合并回主分支..."
git checkout main
git merge "$SYNC_BRANCH" -m "sync: upstream merged"
git branch -D "$SYNC_BRANCH"

echo ""
echo "=== 同步完成 ==="
echo ""
echo "✓ 上游代码已合并到主分支"
echo "✓ 同步分支已清理"
echo ""
echo "下一步操作："
echo "1. 运行测试：cd mytest && go test ./..."
echo "2. 提交更改：git push origin main"
echo ""
