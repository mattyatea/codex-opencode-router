# AGENTS.md

このリポジトリは、Codex のモデル一覧に ChatGPT のモデルと OpenCode Go のモデルを同時に出すためのローカル中継サーバー（`codex-go-router`）です。あわせて、ChatGPT の利用枠が上限でも Codex Desktop の送信ボタンが無効化されないように、バックエンド API の中継（利用枠レスポンスの書き換え）も行います。仕組みと設定の詳細は [README.md](README.md) を参照してください。

## セットアップを依頼されたときの手順

ユーザーから

> https://github.com/mattyatea/codex-opencode-router のセットアップをして

のように依頼されたら、このファイルの手順を最後まで実行してください。

### 完了条件

以下がすべて満たされたら完了です。

1. `~/.codex/codex-go-router` にリポジトリが clone されている
2. `~/.codex/bin/codex-go-router` がビルドされている
3. ルーターが常駐し、`127.0.0.1:18789` で待ち受けている
4. `~/.codex/.env` に `OPENCODE_GO_API_KEY` がある（パーミッション 0600）
5. `~/.codex/config.toml` が `local-router` プロバイダを向いている
6. `codex debug models` の一覧に `go-deepseek-v4.1-flash` / `go-gpt-6-luna` / `go-muse-spark-1.3-contributor` が出る
7. `codex exec --model go-gpt-6-luna 'Reply with OK only.'` が応答する
8. `CODEX_API_BASE_URL=http://localhost:8000/backend-api` がデスクトップアプリに渡る設定になっている（macOS は `launchctl getenv`）
9. デスクトップアプリ再起動後、`~/.codex/codex-go-router.log` に `POST /backend-api/devicecheck -> 200` が出る（アプリがローカル中継を向いている）

### 0. 前提を確認する

```sh
codex --version
go version || echo "go missing"
codex login status
```

- `go` が無ければインストールする（mise / asdf / apt / brew など、その環境の方法で）。
- `codex login status` が ChatGPT ログイン済みでなければ、セットアップの最後に `codex login` をユーザーへ依頼する。通常の GPT モデルには ChatGPT ログインが必要で、OpenCode Go のモデルだけならログインなしでも動く。

### 1. clone してビルドする

```sh
git clone https://github.com/mattyatea/codex-opencode-router.git ~/.codex/codex-go-router
cd ~/.codex/codex-go-router
go build -o ~/.codex/bin/codex-go-router .
```

### 2. OpenCode Go の API キーを設定する

`~/.codex/.env` にすでに `OPENCODE_GO_API_KEY` があればそのまま使う。無ければ**ユーザーにキーを確認してから**次を実行する（キーを推測したり、ログやチャットに残したりしない）。

```sh
umask 077
printf 'export OPENCODE_GO_API_KEY=%s\n' "<API キー>" >> ~/.codex/.env
```

### 3. ルーターを常駐させる

その環境で使える方法を上から順に選ぶ。

**systemd のユーザーサービス（Linux で推奨）**

```sh
mkdir -p ~/.config/systemd/user
cp ~/.codex/codex-go-router/deploy/codex-go-router.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now codex-go-router.service
systemctl --user is-active codex-go-router.service
```

**launchd（macOS）** … `~/Library/LaunchAgents/` に `~/.codex/bin/codex-go-router` を起動する plist を作って `launchctl load` する。

**どちらも無い場合** … `nohup ~/.codex/bin/codex-go-router >> ~/.codex/codex-go-router.log 2>&1 &` で常駐させ、可能ならシェルの起動処理やその環境のプロセス管理に登録する。

### 4. Codex の設定を local-router に向ける

`~/.codex/config.toml` を**必ずバックアップしてから**編集する。

```sh
cp ~/.codex/config.toml ~/.codex/config.toml.bak-codex-opencode-router
```

トップレベル（最初の `[table]` より前）に次を追加する。

```toml
model_provider = "local-router"
model_reasoning_summary = "auto"
```

末尾に次を追加する。

```toml
[model_providers.local-router]
name = "Local router"
base_url = "http://127.0.0.1:18789"
requires_openai_auth = true
wire_api = "responses"
supports_websockets = false
```

既存の `model_provider` や同じ名前のプロバイダ定義がある場合は、黙って置き換えず、**ユーザーに確認してから**上書きする。

**注意:** `chatgpt_base_url`（config）や `CODEX_APP_SERVER_CHATGPT_BASE_URL`（環境変数）は設定しない。Codex Desktop のサインインが解除される。

### 4.5 デスクトップアプリの API ベースをローカル中継へ向ける

macOS:

```sh
launchctl setenv CODEX_API_BASE_URL "http://localhost:8000/backend-api"
cp ~/.codex/codex-go-router/deploy/com.codex.go-router-env.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.codex.go-router-env.plist
```

Linux はデスクトップエントリやシェル起動処理で `export CODEX_API_BASE_URL=http://localhost:8000/backend-api` を設定する。パスは必ず `/backend-api` まで含める（欠けると ChatGPT のログインが壊れる）。

### 5. 検証する

```sh
codex debug models   # go-* のモデルが一覧に出る
codex exec --model go-gpt-6-luna 'Reply with OK only.'
codex exec --model go-deepseek-v4.1-flash 'How much is 17*23? Think briefly.'
```

`codex exec` が動かない場合は `~/.codex/codex-go-router.log` か `systemctl --user status codex-go-router.service` のログを見て、原因を直してから再検証する。

デスクトップアプリの検証は次を確認する。

```sh
launchctl getenv CODEX_API_BASE_URL     # http://localhost:8000/backend-api になっている
tail -20 ~/.codex/codex-go-router.log   # /backend-api/devicecheck や /backend-api/wham/usage が出る
```

アプリ側で利用枠 API が `allowed: true` に書き換わっていることを確認するには、app-server へ直接問い合わせる。

```sh
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"probe","title":"Probe","version":"0.0.1"}}}' \
  '{"jsonrpc":"2.0","method":"initialized","params":{}}' \
  '{"jsonrpc":"2.0","id":2,"method":"account/rateLimits/read"}' \
  | codex app-server 2>/dev/null | grep '"id":2'
```

### 6. 反映する

Codex デスクトップアプリや app-server が起動中なら再起動して設定を読み込ませる。CLI は次回起動から反映される。

### 7. 報告する

実行したコマンド、`codex debug models` の結果、動かなかった点と対処をユーザーに報告する。Muse Spark を使う場合だけ、後述の Privacy 設定が必要なことを伝える。

### ユーザーに確認すべきこと

- OpenCode Go の API キー（`~/.codex/.env` に未設定の場合のみ）
- 既存の `model_provider` や同名プロバイダ定義を置き換えてよいか

### つまずきやすい点

| 症状 | 原因と対処 |
| --- | --- |
| `codex debug models` に `go-*` が出ない | ルーターが起動していない。`systemctl --user status codex-go-router.service` とログを確認する |
| 通常の GPT モデルが `429 usage_limit_reached` | ChatGPT の使用枠上限。セットアップの失敗ではない。枠のリセットを待つ |
| Muse Spark が 400 | OpenCode のワークスペース設定で「学習に使われる有料エンドポイントを許可」をオンにする（https://dev.opencode.ai/auth ） |
| `codex exec` が 401 | ルーターの `/models` は ChatGPT の `Authorization` が必要。`codex login` 済みか確認する |
| `go build` が失敗する | Go のバージョンが古い。1.22 以上にする |
| ポート 18789 が使用中 | `ROUTER_LISTEN=127.0.0.1:18790` で起動し、`config.toml` の `base_url` も合わせる |
| ポート 8000 が使用中 | `ROUTER_API_LISTEN=127.0.0.1:8001` で起動し、`CODEX_API_BASE_URL` も `http://localhost:8001/backend-api` に合わせる |
| デスクトップで送信ボタンがグレーアウトしたまま | `CODEX_API_BASE_URL` が未設定、または起動中アプリに反映されていない。設定後にアプリを再起動する |
| ChatGPT タブがサインイン画面になる / サインアウトされる | `chatgpt_base_url`（config）や `CODEX_APP_SERVER_CHATGPT_BASE_URL` を設定している。削除してアプリを再起動する |
| ログに `/backend-api/devicecheck` が出ない | アプリが中継を向いていない。`launchctl getenv CODEX_API_BASE_URL` を確認し、アプリを再起動する |

## リポジトリの構成

- `main.go` … ルーター本体。`GO_MODELS` に対応モデルを定義している
- `api.go` … ChatGPT API 中継（`CODEX_API_BASE_URL` の受け先）。利用枠レスポンスだけを書き換える
- `main_test.go` / `api_test.go` … カタログ合成、reasoning 変換、利用枠サニタイズのユニットテスト
- `deploy/codex-go-router.service` … systemd ユーザーサービスの雛形
- `deploy/com.codex.go-router-env.plist` … macOS で `CODEX_API_BASE_URL` を永続化する LaunchAgent の雛形
- `README.md` … 仕組み、手動セットアップ、設定一覧、注意点
