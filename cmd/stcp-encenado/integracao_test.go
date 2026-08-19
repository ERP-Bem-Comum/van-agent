package main_test

// A prova que nenhum outro teste dá: o agente de verdade acionando ESTE binário, como processo.
//
// Os testes de `main_test.go` exercitam as funções. Estes compilam o programa e o colocam no lugar
// do cliente do banco, com `stcp.NewCommandClient` — o mesmo caminho de produção. É o único jeito de
// provar que a costura entre os dois fecha: `exec` de verdade, argumentos de verdade, ambiente
// herdado de verdade, código de saída de verdade.
//
// Sem isto, o defeito só apareceria em quem for montar a simulação — que é exatamente para quem este
// binário existe.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"

	"github.com/ERP-Bem-Comum/van-agent/internal/agent"
	"github.com/ERP-Bem-Comum/van-agent/internal/bucket"
	"github.com/ERP-Bem-Comum/van-agent/internal/envelope"
	"github.com/ERP-Bem-Comum/van-agent/internal/ledger"
	"github.com/ERP-Bem-Comum/van-agent/internal/spool"
	"github.com/ERP-Bem-Comum/van-agent/internal/stcp"
)

const (
	remessaIntegracao = "PAG_000000.20260818120000_0001.REM"
	retornoIntegracao = "PAG_000000.20260818110000_0001.RET"
)

var padraoIntegracao = regexp.MustCompile(`^PAG_\d+\.\d+_\d+\.REM$`)

// O ciclo de transmissão inteiro, com o binário no lugar do cliente do banco.
func TestAgenteTransmiteAcionandoOBinarioDeVerdade(t *testing.T) {
	amb := novoAmbiente(t)
	ag, store := amb.montarAgente(t)

	store.Seed(bucket.DefaultPrefixes().Outbound+remessaIntegracao, []byte("conteúdo fictício"))

	sum, err := ag.TransmitCycle(context.Background())
	if err != nil {
		t.Fatalf("ciclo abortou: %v", err)
	}
	if errs := sum.Errs(); len(errs) > 0 {
		t.Fatalf("o ciclo acumulou erros: %v", errs)
	}
	if len(sum.Outcomes) != 1 {
		t.Fatalf("esperava 1 desfecho, veio %d", len(sum.Outcomes))
	}

	out := sum.Outcomes[0]
	if !out.ClientInvoked {
		t.Fatal("o cliente não foi acionado; sem isso a encenação não provou nada")
	}
	// O veredito vem da EVIDÊNCIA FÍSICA (ADR-0061 §2), e quem a produziu foi o processo externo.
	if out.Situation != envelope.Transmitted {
		t.Errorf("situação = %q, esperava %q · o binário não produziu a evidência que o agente lê",
			out.Situation, envelope.Transmitted)
	}

	env := lerEnvelope(t, store, envelope.Key(remessaIntegracao))
	if env.Situacao != envelope.Transmitted {
		t.Errorf("envelope publicado com situação %q", env.Situacao)
	}
	// As linhas cruas vieram do log que o processo externo escreveu, e passaram pelo parser do §12.
	if len(env.LogTransferencia) == 0 {
		t.Error("o envelope saiu sem linha de log; o binário não escreveu o log posicional, " +
			"ou o parser não o entendeu")
	}
}

// O ciclo de recepção — o modo que não tinha sido exercitado fora da suíte, e que era a P9 do
// core-api.
func TestAgenteRecebeAcionandoOBinarioDeVerdade(t *testing.T) {
	amb := novoAmbiente(t)
	amb.enfileiraEntrega(t, retornoIntegracao, "conteúdo de retorno fictício")
	ag, store := amb.montarAgente(t)

	sum, err := ag.ReceiveCycle(context.Background())
	if err != nil {
		t.Fatalf("ciclo de recepção abortou: %v", err)
	}
	if errs := sum.Errs(); len(errs) > 0 {
		t.Fatalf("o ciclo acumulou erros: %v", errs)
	}
	if len(sum.Outcomes) != 1 {
		t.Fatalf("esperava 1 arquivo recebido, veio %d", len(sum.Outcomes))
	}

	out := sum.Outcomes[0]
	if out.FileName != retornoIntegracao {
		t.Errorf("recebeu %q, esperava %q", out.FileName, retornoIntegracao)
	}
	// A correlação exige que o binário tenha escrito a linha de recepção com carimbo DENTRO da janela
	// deste ciclo. É a costura mais fina entre os dois, e a que mais silenciosamente falharia.
	if !sum.LogDoCicloLido {
		t.Error("o log deste ciclo não foi lido; o binário não escreveu no caminho esperado, " +
			"ou o carimbo caiu fora da janela da execução")
	}
	if !out.Correlated {
		t.Error("o arquivo não correlacionou com o log; a linha de recepção não foi reconhecida")
	}

	env := lerEnvelope(t, store, out.StatusKey)
	if env.Recepcao == nil || !env.Recepcao.LogDoCicloLido || !env.Recepcao.Correlacionado {
		t.Errorf("o envelope não declarou a proveniência: %+v", env.Recepcao)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Ambiente
// ─────────────────────────────────────────────────────────────────────────────

type ambiente struct {
	raiz, saida, backup, entrada, recebidos, entregarDe, logDir, exe string
}

func novoAmbiente(t *testing.T) *ambiente {
	t.Helper()
	if testing.Short() {
		t.Skip("compila o binário; pulado em -short")
	}

	raiz := t.TempDir()
	a := &ambiente{
		raiz:       raiz,
		saida:      filepath.Join(raiz, "SAIDA"),
		backup:     filepath.Join(raiz, "BACKUP"),
		entrada:    filepath.Join(raiz, "ENTRADA"),
		recebidos:  filepath.Join(raiz, "RECEBIDOS"),
		entregarDe: filepath.Join(raiz, "ENTREGAR"),
		logDir:     filepath.Join(raiz, "LOG"),
	}
	for _, d := range []string{a.saida, a.backup, a.entrada, a.recebidos, a.entregarDe, a.logDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("preparar %s: %v", d, err)
		}
	}
	a.exe = a.compilar(t)

	// O subprocesso herda o ambiente do processo de teste, então é aqui que a encenação é
	// configurada — do mesmo jeito que uma instalação real a configuraria.
	t.Setenv("STCP_ENCENADO_CONFIRMO", "nao-transmite-nada")
	t.Setenv("STCP_ENCENADO_OUTBOUND_DIR", a.saida)
	t.Setenv("STCP_ENCENADO_BACKUP_DIR", a.backup)
	t.Setenv("STCP_ENCENADO_INBOUND_DIR", a.entrada)
	t.Setenv("STCP_ENCENADO_ENTREGAR_DE", a.entregarDe)
	t.Setenv("STCP_ENCENADO_LOG_PATH", filepath.Join(a.logDir, "20260818.LOG"))
	return a
}

// compilar produz o executável a partir do código deste pacote.
//
// Compilar em vez de apontar para um binário pré-existente é o que mantém o teste honesto: ele
// exercita o código desta árvore, não o que alguém deixou instalado na máquina.
func (a *ambiente) compilar(t *testing.T) string {
	t.Helper()

	exe := filepath.Join(a.raiz, "stcp-encenado")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}

	cmd := exec.Command("go", "build", "-o", exe, "github.com/ERP-Bem-Comum/van-agent/cmd/stcp-encenado")
	if saida, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("não foi possível compilar o binário de encenação (toolchain indisponível?): %v\n%s", err, saida)
	}
	return exe
}

func (a *ambiente) enfileiraEntrega(t *testing.T, nome, conteudo string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(a.entregarDe, nome), []byte(conteudo), 0o640); err != nil {
		t.Fatalf("enfileirar entrega: %v", err)
	}
}

// montarAgente usa os componentes REAIS — ledger em disco, pastas em disco, cliente por linha de
// comando. O único duplo é o bucket, porque ele não é o que este teste está exercitando.
func (a *ambiente) montarAgente(t *testing.T) (*agent.Agent, *bucket.Memory) {
	t.Helper()

	led, err := ledger.NewFileLedger(filepath.Join(a.raiz, "ledger"))
	if err != nil {
		t.Fatalf("registro: %v", err)
	}
	index, err := ledger.NewFileReceptionIndex(filepath.Join(a.raiz, "ledger", "recepcao"))
	if err != nil {
		t.Fatalf("índice de recepção: %v", err)
	}
	pending, err := ledger.NewFilePendingEnvelopes(filepath.Join(a.raiz, "ledger", "pendentes"))
	if err != nil {
		t.Fatalf("pendências: %v", err)
	}

	sp, err := spool.NewDir(spool.Config{
		OutboundDir: a.saida, BackupDir: a.backup, InboundDir: a.entrada,
		ReceivedDir: a.recebidos, LogDir: a.logDir, TransferLogGlob: "*.LOG",
	})
	if err != nil {
		t.Fatalf("pastas: %v", err)
	}

	client, err := stcp.NewCommandClient(stcp.CommandConfig{
		ExecutablePath:       a.exe,
		ConfigPath:           filepath.Join(a.raiz, "config.ini"),
		Profile:              "PERFIL-DE-TESTE",
		Retries:              1,
		RetryIntervalSeconds: 1,
	})
	if err != nil {
		t.Fatalf("cliente: %v", err)
	}

	store := bucket.NewMemory()
	// Relógio real, e não injetado: o carimbo das linhas de log vem do PROCESSO EXTERNO, e um relógio
	// congelado aqui colocaria a janela do ciclo num tempo que o binário não conhece — a correlação
	// falharia por artifício do teste, não por defeito.
	ag, err := agent.New(store, led, client, sp, agent.Config{
		Prefixes:    bucket.DefaultPrefixes(),
		NamePattern: padraoIntegracao,
		Clock:       time.Now,
	}, agent.WithReceptionIndex(index), agent.WithPendingEnvelopes(pending))
	if err != nil {
		t.Fatalf("montar agente: %v", err)
	}
	return ag, store
}

func lerEnvelope(t *testing.T, store *bucket.Memory, key string) envelope.Envelope {
	t.Helper()
	raw, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("envelope ausente em %q; chaves: %v", key, store.Keys())
	}
	var env envelope.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope ilegível: %v", err)
	}
	return env
}
