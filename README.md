# codex-opencode-router

Codex はプロバイダを同時に1つしか使えないため、モデル一覧には「ChatGPT のモデル」か「OpenCode Go のモデル」のどちらかしか出せません。このリポジトリは、その制約をループバックの中継サーバーで回避する小さなリレーです。

- `GET /models` … ChatGPT のモデルカタログを取得し、OpenCode Go のモデルを追記して返す
- `POST /responses` … モデル名が `go-` で始まれば OpenCode Go へ、それ以外は ChatGPT へ転送する

Codex 側は `model_provider = "local-router"` を向くだけで、通常の GPT モデルと OpenCode Go のモデルを同じモデル一覧から選べます。

```mermaid
flowchart LR
  Codex[Codex CLI / Desktop] -->|"GET /models"| Router[codex-go-router<br/>127.0.0.1:18789]
  Codex -->|"POST /responses"| Router
  Router -->|"go-* を転送"| Go[OpenCode Go<br/>opencode.ai/zen/go/v1]
  Router -->|"それ以外を転送"| ChatGPT[ChatGPT backend<br/>chatgpt.com/backend-api/codex]
  Router -.->|"モデル一覧を合成"| Codex
```

## 前提

- Codex CLI がインストール済みで `codex login` 済み
- OpenCode Go のサブスクリプションと API キー
- Go 1.22 以上（ビルド用）

## AI エージェントにセットアップさせるための依頼文

以下をそのまま AI エージェント（Codex など）に貼り付けてください。`<...>` は環境に合わせて埋めます。

````text
あなたはこのマシンに codex-opencode-router をセットアップします。
作業前に ~/.codex/config.toml をバックアップし、既存の設定を壊さないでください。

1. Go ツールチェーンを確認する（無ければインストールする）。
2. `git clone https://github.com/mattyatea/codex-opencode-router.git ~/.codex/codex-go-router` して、
   `cd ~/.codex/codex-go-router && go build -o ~/.codex/bin/codex-go-router .` でビルドする。
3. `~/.codex/.env` に `export OPENCODE_GO_API_KEY=<OpenCode Go の API キー>` を書き、
   パーミッションを 0600 にする。キーはユーザーに確認する（既存の設定があれば流用する）。
4. Linux なら `~/.config/systemd/user/codex-go-router.service` を作成し、
   `systemctl --user daemon-reload && systemctl --user enable --now codex-go-router.service` する。
   systemd のユーザーサービスが使えない環境では `nohup ~/.codex/bin/codex-go-router >/dev/null 2>&1 &`
   などで常駐させる。macOS なら launchd の plist を使う。
5. `~/.codex/config.toml` のトップレベルに `model_provider = "local-router"` を追加し、
   末尾に次の provider テーブルを追加する:

   [model_providers.local-router]
   name = "Local router"
   base_url = "http://127.0.0.1:18789"
   requires_openai_auth = true
   wire_api = "responses"
   supports_websockets = false

6. 思考サマリーを表示したい場合は `model_reasoning_summary = "auto"` も追加する。
7. `codex debug models` を実行し、go-deepseek-v4.1-flash / go-gpt-6-luna /
   go-muse-spark-1.3-contributor が一覧に出ることを確認する。
8. `codex exec --model go-gpt-6-luna 'Reply with OK only.'` で応答を確認する。
9. 既存の Codex プロセス（デスクトップアプリや app-server）を再起動して設定を反映する。
10. 完了したら、実行したコマンド、`codex debug models` の結果、つまずいた点を報告する。
````

## 手動セットアップ（Linux の例）

```sh
git clone https://github.com/mattyatea/codex-opencode-router.git ~/.codex/codex-go-router
cd ~/.codex/codex-go-router
go build -o ~/.codex/bin/codex-go-router .

# API キー（Codex 本体と同じ .env を読む）
umask 077
printf 'export OPENCODE_GO_API_KEY=%s\n' "$OPENCODE_GO_API_KEY" >> ~/.codex/.env

mkdir -p ~/.config/systemd/user
cp deploy/codex-go-router.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now codex-go-router.service
systemctl --user status codex-go-router.service
```

`~/.codex/config.toml` に追加する設定:

```toml
model_provider = "local-router"
model_reasoning_summary = "auto"

[model_providers.local-router]
name = "Local router"
base_url = "http://127.0.0.1:18789"
requires_openai_auth = true
wire_api = "responses"
supports_websockets = false
```

## 動作確認

```sh
codex debug models        # go-* のモデルが一覧に出る
codex exec --model go-gpt-6-luna 'Reply with OK only.'
codex exec --model go-deepseek-v4.1-flash 'How much is 17*23? Think briefly.'
```

## 設定

| 変数 | 既定値 | 説明 |
| --- | --- | --- |
| `OPENCODE_GO_API_KEY` | なし（必須） | `~/.codex/.env`、無ければプロセス環境変数から読む |
| `ROUTER_LISTEN` | `127.0.0.1:18789` | 待ち受けアドレス |
| `CODEX_HOME` | `~/.codex` | `.env` を探すディレクトリ |

## 対応モデル

`main.go` の `GO_MODELS` に定義しています。OpenCode Go 側で Responses API に対応しているモデルだけを載せてください（chat/completions 専用のモデルは `/responses` では 503 になります）。

| モデル ID | Codex での表示名 | 備考 |
| --- | --- | --- |
| `deepseek-v4.1-flash` | DeepSeek V4.1 Flash (OpenCode Go) | 生の reasoning を思考サマリーに変換して表示 |
| `gpt-6-luna` | GPT 6 Luna (OpenCode Go) | ネイティブの reasoning summary に対応 |
| `muse-spark-1.3-contributor` | opencode-go/muse-spark-1.3 (Train) | ワークスペースの Privacy 設定で学習許可が必要 |

## 注意

- OpenCode Go のモデルは `go-` 接頭辞で識別します。ChatGPT 側のカタログに同じ ID があっても衝突しません。
- DeepSeek 系は `reasoning_text` イベントを返すため、ルーターが Codex の描画する `reasoning_summary` イベントへ変換しています。
- Muse Spark は OpenCode のワークスペース設定で「学習に使われる有料エンドポイントを許可」をオンにしないと 400 になります。
- ChatGPT の使用枠が上限に達していると、通常の GPT モデルは上流の 429 をそのまま返します。
- ルーターは 127.0.0.1 のみで待ち受け、認証はありません。マルチユーザーマシンでは注意してください。

## 開発

```sh
go vet ./...
go test ./...
```
