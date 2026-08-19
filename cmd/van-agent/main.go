// Comando van-agent — o transporte entre o bucket e o cliente OFTP do banco.
//
// O processo é ONE-SHOT: roda um ciclo e sai. Quem agenda é o Agendador de Tarefas do Windows, como
// o fabricante documenta (§10, pp. 20-23) — não há laço interno, não há serviço. A consequência
// prática é que o código de saída é a interface com o agendador, e por isso ele é explícito.
//
// Dois modos, e a diferença entre eles é UMA linha: qual armazenamento o ciclo recebe.
//
//   - `ensaio` roda o ciclo inteiro contra um armazenamento em memória. Serve para verificar uma
//     instalação nova — configuração, pastas, executável, filtro, registro — sem que nada precise
//     existir no bucket, nem credencial, nem rede.
//   - `transmissao` roda contra o bucket real.
//
// Que os dois compartilhem o MESMO ciclo é deliberado: um ensaio que exercitasse um caminho
// diferente do de produção verificaria o ensaio, não a instalação.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
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
	mode := flag.String("modo", "", "ensaio | transmissao")
	flag.Parse()

	switch *mode {
	case "ensaio":
		os.Exit(cycle(*mode, rehearsalStore))
	case "transmissao":
		os.Exit(cycle(*mode, realStore))
	default:
		fmt.Fprintln(os.Stderr, "uso: van-agent -modo=ensaio | van-agent -modo=transmissao")
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

	ag, err := agent.New(store, led, client, sp, agent.Config{
		Prefixes:    cfg.Prefixes,
		NamePattern: cfg.NamePattern,
		Clock:       time.Now,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "montar agente: %v\n", err)
		return exitConfig
	}

	summary, err := ag.TransmitCycle(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ciclo: %v\n", err)
		return exitSoftware
	}

	report(mode, storeLabel, cfg.NamePattern, summary)
	if errs := summary.Errs(); len(errs) > 0 {
		fmt.Fprintf(os.Stderr, "o ciclo acumulou %d erro(s): %v\n", len(errs), errors.Join(errs...))
		return exitSoftware
	}
	return exitOK
}

func report(mode, storeLabel string, pattern *regexp.Regexp, summary agent.Summary) {
	fmt.Printf("modo %s concluído · %s · padrão de nome: %s\n", mode, storeLabel, pattern)
	fmt.Printf("objetos na fila: %d\n", len(summary.Outcomes))
	for _, o := range summary.Outcomes {
		fmt.Printf("  %-40s situação=%-12s cliente acionado=%v\n",
			o.FileName, situationOrDash(o.Situation), o.ClientInvoked)
	}
}

func situationOrDash(s envelope.Situation) string {
	if s == "" {
		return "—"
	}
	return string(s)
}
