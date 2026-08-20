// Comando van-agent — o transporte entre o bucket e o cliente OFTP do banco.
//
// O processo é ONE-SHOT: roda um ciclo e sai. Quem agenda é o Agendador de Tarefas do Windows, como
// o fabricante documenta (§10, pp. 20-23) — não há laço interno, não há serviço. A consequência
// prática é que o código de saída é a interface com o agendador, e por isso ele é explícito.
//
// Três modos. Entre `ensaio` e `transmissao` a diferença é UMA linha — qual armazenamento o ciclo
// recebe; `recepcao` é o outro ciclo.
//
//   - `ensaio` roda o ciclo inteiro contra um armazenamento em memória. Serve para verificar uma
//     instalação nova — configuração, pastas, executável, filtro, registro — sem que nada precise
//     existir no bucket, nem credencial, nem rede.
//   - `transmissao` roda contra o bucket real.
//   - `recepcao` traz o que o banco enviou e o deposita no bucket. É o ÚNICO ciclo que não paga
//     ninguém se der errado, e por isso é por ele que se começa a exercitar qualquer instalação
//     real.
//
// Que `ensaio` e `transmissao` compartilhem o MESMO ciclo é deliberado: um ensaio que exercitasse um
// caminho diferente do de produção verificaria o ensaio, não a instalação.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/ERP-Bem-Comum/van-agent/internal/agent"
	"github.com/ERP-Bem-Comum/van-agent/internal/bucket"
	"github.com/ERP-Bem-Comum/van-agent/internal/config"
	"github.com/ERP-Bem-Comum/van-agent/internal/envelope"
	"github.com/ERP-Bem-Comum/van-agent/internal/ledger"
	"github.com/ERP-Bem-Comum/van-agent/internal/spool"
	"github.com/ERP-Bem-Comum/van-agent/internal/stcp"
)

// Códigos de saída no espírito do sysexits.h, para que o agendador distinga o que exige intervenção
// do que é apenas ruído de um ciclo.
const (
	exitOK       = 0
	exitSoftware = 70 // erro de execução
	exitConfig   = 78 // configuração incompleta ou inválida
)

// storeBuilder monta o armazenamento do modo, e devolve como ele deve aparecer no relatório —
// porque a única diferença que importa entre os dois modos precisa estar visível na saída de quem
// rodou.
type storeBuilder func(ctx context.Context) (bucket.Store, string, error)

func main() {
	// A lista aqui é a que o `-h` imprime, e por isso ela precisa ser a MESMA do `switch` abaixo e do
	// texto de uso: um modo que existe e não aparece no `-h` é um modo que ninguém descobre. Foi o que
	// aconteceu com `recepcao` — justamente o único ciclo que não move dinheiro, e por isso o primeiro
	// a ser rodado numa instalação nova.
	mode := flag.String("modo", "", "ensaio | transmissao | recepcao")
	flag.Parse()

	switch *mode {
	case "ensaio":
		os.Exit(cycle(*mode, rehearsalStore))
	case "transmissao":
		os.Exit(cycle(*mode, realStore))
	case "recepcao":
		os.Exit(receive())
	default:
		fmt.Fprintln(os.Stderr,
			"uso: van-agent -modo=ensaio | van-agent -modo=transmissao | van-agent -modo=recepcao")
		os.Exit(exitConfig)
	}
}

// rehearsalStore é o duplo em memória: o ensaio não lê nem escreve no armazenamento real.
func rehearsalStore(context.Context) (bucket.Store, string, error) {
	return bucket.NewMemory(), "armazenamento em memória (nada é lido nem escrito no bucket)", nil
}

// realStore é o bucket. A configuração dele é lida SÓ aqui, e não no boot comum, para que o ensaio
// continue rodável numa instalação sem bucket atribuído.
func realStore(ctx context.Context) (bucket.Store, string, error) {
	storageCfg, err := config.LoadStorage()
	if err != nil {
		return nil, "", err
	}
	store, err := bucket.NewS3(ctx, storageCfg)
	if err != nil {
		return nil, "", err
	}
	// O nome do bucket NÃO entra no relatório: ele identifica o convênio, e a saída do agente vai
	// para o log do agendador numa máquina que ninguém rotaciona.
	return store, "bucket real", nil
}

// cycle roda uma passada de transmissão.
//
// ⚠️ Os DOIS modos ACIONAM o cliente STCP de verdade. Se houver arquivo na pasta de SAÍDA da
// instalação, ele será transmitido — porque é isso que o cliente faz (§5, p.13). Rodar ensaio numa
// instalação de produção com a fila suja transmite pagamento real; o que o ensaio evita é o bucket,
// não a fila do banco.
func cycle(mode string, buildStore storeBuilder) int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuração: %v\n", err)
		return exitConfig
	}

	led, err := ledger.NewFileLedger(cfg.LedgerDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "registro de intenção: %v\n", err)
		return exitConfig
	}

	sp, err := spool.NewDir(cfg.Spool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pastas do cliente STCP: %v\n", err)
		return exitConfig
	}

	client, err := stcp.NewCommandClient(cfg.Client)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cliente STCP: %v\n", err)
		return exitConfig
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, storeLabel, err := buildStore(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "armazenamento: %v\n", err)
		return exitConfig
	}

	pending, err := ledger.NewFilePendingEnvelopes(filepath.Join(cfg.LedgerDir, "pendentes"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "registro de envelopes pendentes: %v\n", err)
		return exitConfig
	}

	ag, err := agent.New(store, led, client, sp, agent.Config{
		Prefixes:      cfg.Prefixes,
		NamePattern:   cfg.NamePattern,
		NameMaxLength: cfg.NameMaxLength,
		Clock:         time.Now,
	}, agent.WithPendingEnvelopes(pending))
	if err != nil {
		fmt.Fprintf(os.Stderr, "montar agente: %v\n", err)
		return exitConfig
	}

	summary, err := ag.TransmitCycle(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ciclo: %v\n", err)
		return exitSoftware
	}

	report(mode, storeLabel, cfg.NamePattern, cfg.NameMaxLength, summary)
	if errs := summary.Errs(); len(errs) > 0 {
		relatarErros(errs)
		return exitSoftware
	}
	return exitOK
}

// receive roda uma passada de RECEPÇÃO contra o bucket real.
//
// Não há variante de ensaio: o ciclo de recepção não deposita nada na fila do banco, então o risco
// que o ensaio existe para evitar não se aplica aqui.
func receive() int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuração: %v\n", err)
		return exitConfig
	}
	if err := cfg.Spool.ValidateReception(); err != nil {
		fmt.Fprintf(os.Stderr,
			"configuração da recepção: %v (defina %sSTCP_INBOUND_DIR e %sSTCP_RECEIVED_DIR)\n",
			err, "VAN_AGENT_", "VAN_AGENT_")
		return exitConfig
	}

	led, err := ledger.NewFileLedger(cfg.LedgerDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "registro de intenção: %v\n", err)
		return exitConfig
	}

	// Diretório PRÓPRIO para o índice de recepção: ele indexa hash de conteúdo, o outro indexa nome
	// de remessa, e uma pasta separada torna a colisão entre os dois impossível em vez de improvável.
	index, err := ledger.NewFileReceptionIndex(filepath.Join(cfg.LedgerDir, "recepcao"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "índice de recepção: %v\n", err)
		return exitConfig
	}

	sp, err := spool.NewDir(cfg.Spool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pastas do cliente STCP: %v\n", err)
		return exitConfig
	}

	client, err := stcp.NewCommandClient(cfg.Client)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cliente STCP: %v\n", err)
		return exitConfig
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, _, err := realStore(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "armazenamento: %v\n", err)
		return exitConfig
	}

	// Mesmo raciocínio do índice: diretório próprio, porque aqui se indexa a CHAVE do envelope.
	pending, err := ledger.NewFilePendingEnvelopes(filepath.Join(cfg.LedgerDir, "pendentes"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "registro de envelopes pendentes: %v\n", err)
		return exitConfig
	}

	ag, err := agent.New(store, led, client, sp, agent.Config{
		Prefixes:      cfg.Prefixes,
		NamePattern:   cfg.NamePattern,
		NameMaxLength: cfg.NameMaxLength,
		Clock:         time.Now,
	}, agent.WithReceptionIndex(index), agent.WithPendingEnvelopes(pending))
	if err != nil {
		fmt.Fprintf(os.Stderr, "montar agente: %v\n", err)
		return exitConfig
	}

	summary, err := ag.ReceiveCycle(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ciclo de recepção: %v\n", err)
		return exitSoftware
	}

	reportReception(summary)
	if errs := summary.Errs(); len(errs) > 0 {
		relatarErros(errs)
		return exitSoftware
	}
	return exitOK
}

// relatarErros imprime os erros do ciclo, SEPARANDO os que nenhum ciclo seguinte vai retomar.
//
// A separação existe porque as duas categorias pedem ações opostas: um erro comum de publicação é
// "o próximo ciclo resolve", e um envelope órfão é "vá olhar o bucket agora, ou o desfecho de um
// pagamento fica perdido". Misturados na mesma lista, o grave lê como rotina — que é exatamente
// como um item importante passa despercebido numa saída com vinte linhas.
//
// O código de saída continua sendo o mesmo (70): quem agenda não distingue categorias, e inventar
// um código novo quebraria o contrato do Agendador de Tarefas por uma informação que pertence ao
// texto, não ao status.
func relatarErros(errs []error) {
	fmt.Fprintf(os.Stderr, "o ciclo acumulou %d erro(s): %v\n", len(errs), errors.Join(errs...))

	var orfaos []error
	for _, e := range errs {
		if errors.Is(e, agent.ErrOrphanEnvelope) {
			orfaos = append(orfaos, e)
		}
	}
	if len(orfaos) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr,
		"\n⚠️  AÇÃO HUMANA NECESSÁRIA — %d desfecho(s) sem publicação e sem retomada.\n"+
			"Nenhum ciclo seguinte resolve isto sozinho; há objeto no bucket que o backend não\n"+
			"consegue interpretar, e ele é indistinguível de um arquivo que nunca deveria ter entrado.\n%v\n",
		len(orfaos), errors.Join(orfaos...))
}

func reportReception(summary agent.ReceiveSummary) {
	fmt.Printf("modo recepcao concluído · arquivos na pasta de entrada: %d\n", len(summary.Outcomes))
	if summary.Republicados > 0 {
		fmt.Printf("⚠️  envelopes de ciclos anteriores republicados agora: %d\n", summary.Republicados)
	}
	if !summary.LogDoCicloLido {
		// Não é erro, e por isso precisa ser dito: o ciclo funciona sem o log, mas nenhuma correlação
		// deste ciclo é confiável, e a causa costuma ser o padrão do log apontando para o lugar errado.
		fmt.Println("⚠️  o log DESTA execução não foi lido; nenhuma ausência de correlação abaixo " +
			"diz respeito ao arquivo — conferir VAN_AGENT_STCP_TRANSFER_LOG_GLOB e a pasta de log")
	}
	for _, o := range summary.Outcomes {
		correlacao := "sem correlação no log"
		if o.Correlated {
			correlacao = "correlacionado pelo log"
		}
		fmt.Printf("  %-40s situação=%-12s %s · sha256=%s\n",
			o.FileName, situationOrDash(o.Situation), correlacao, resumoDoHash(o.Sha256))
	}
	// O log diz ter recebido e o arquivo não estava lá: precisa aparecer, porque é o desfecho que
	// ninguém percebe.
	for _, ausente := range summary.LoggedButAbsent {
		fmt.Printf("  ⚠️  %s consta no log deste ciclo e NÃO estava na pasta de entrada\n", ausente)
	}
}

// resumoDoHash encurta o hash para leitura na tela. O valor completo vai para o envelope — aqui é
// só para quem está olhando o console do agendador.
func resumoDoHash(sum string) string {
	if len(sum) <= 12 {
		return sum
	}
	return sum[:12] + "…"
}

func report(mode, storeLabel string, pattern *regexp.Regexp, maxLen int, summary agent.Summary) {
	fmt.Printf("modo %s concluído · %s · padrão de nome: %s · teto de comprimento: %s\n",
		mode, storeLabel, pattern, limitOrNone(maxLen))
	if summary.Republicados > 0 {
		// Sai só quando houve, e destacado: republicação significa que uma remessa transmitida ficou
		// sem desfecho publicado até agora. É informação sobre um ciclo ANTERIOR, e some da vista se
		// aparecer como mais uma linha de rotina.
		fmt.Printf("⚠️  envelopes de ciclos anteriores republicados agora: %d\n", summary.Republicados)
	}
	fmt.Printf("objetos na fila: %d\n", len(summary.Outcomes))
	for _, o := range summary.Outcomes {
		fmt.Printf("  %-40s situação=%-12s cliente acionado=%v\n",
			o.FileName, situationOrDash(o.Situation), o.ClientInvoked)
	}
}

// limitOrNone deixa visível no relatório que a trava está desligada. Quem roda o ensaio numa
// instalação nova precisa ver que ela não está protegendo nada — e não descobrir isso pelo 1101.
func limitOrNone(maxLen int) string {
	if maxLen <= 0 {
		return "sem trava"
	}
	return fmt.Sprintf("%d caracteres", maxLen)
}

func situationOrDash(s envelope.Situation) string {
	if s == "" {
		return "—"
	}
	return string(s)
}
