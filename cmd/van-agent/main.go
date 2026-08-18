// Comando van-agent — o transporte entre o bucket e o cliente OFTP do banco.
//
// O processo é ONE-SHOT: roda um ciclo e sai. Quem agenda é o Agendador de Tarefas do Windows, como
// o fabricante documenta (§10, pp. 20-23) — não há laço interno, não há serviço. A consequência
// prática é que o código de saída é a interface com o agendador, e por isso ele é explícito.
//
// ⚠️ ESTADO DESTA FATIA: só o modo `ensaio` está disponível.
//
// O ciclo real depende do adapter de object storage, que entra em fatia própria com a dependência
// justificada. Esta fatia entrega o NÚCLEO — a ordem das operações, a idempotência e o tratamento de
// execução interrompida — que é onde mora o risco de pagamento duplicado, e o entrega exercitado
// contra um duplo do cliente STCP. O modo `ensaio` existe para que uma instalação nova seja
// verificável (pastas, executável, filtro, registro) sem depositar nada na fila real do banco.
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

func main() {
	mode := flag.String("modo", "", "ensaio | transmissao")
	flag.Parse()

	switch *mode {
	case "ensaio":
		os.Exit(rehearse())
	case "transmissao":
		fmt.Fprintln(os.Stderr,
			"o modo `transmissao` ainda não existe: ele depende do adapter de object storage, que entra "+
				"na próxima fatia. Use `-modo=ensaio` para verificar a instalação sem tocar a fila do banco.")
		os.Exit(exitConfig)
	default:
		fmt.Fprintln(os.Stderr, "uso: van-agent -modo=ensaio")
		os.Exit(exitConfig)
	}
}

// rehearse roda o ciclo completo contra um armazenamento em memória.
//
// O que ele verifica de verdade: que a configuração está completa, que as pastas existem, que o
// executável do cliente responde, que o filtro de nomenclatura casa o que deveria — e que o
// registro de intenção grava em disco. Nada disso depende do bucket, e tudo isso é o que costuma
// estar errado numa instalação nova.
//
// ⚠️ O ensaio ACIONA o cliente STCP de verdade. Se houver arquivo na pasta de SAÍDA da instalação,
// ele será transmitido — porque é isso que o cliente faz (§5, p.13). Rodar ensaio numa instalação
// de produção com a fila suja transmite pagamento real.
func rehearse() int {
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

	// O bucket é o duplo em memória: o ensaio não lê nem escreve no armazenamento real.
	store := bucket.NewMemory()

	ag, err := agent.New(store, led, client, sp, agent.Config{
		Prefixes:    cfg.Prefixes,
		NamePattern: cfg.NamePattern,
		Clock:       time.Now,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "montar agente: %v\n", err)
		return exitConfig
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	summary, err := ag.TransmitCycle(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ciclo: %v\n", err)
		return exitSoftware
	}

	report(cfg.NamePattern, summary)
	if errs := summary.Errs(); len(errs) > 0 {
		fmt.Fprintf(os.Stderr, "o ciclo acumulou %d erro(s): %v\n", len(errs), errors.Join(errs...))
		return exitSoftware
	}
	return exitOK
}

func report(pattern *regexp.Regexp, summary agent.Summary) {
	fmt.Printf("ensaio concluído · padrão de nome: %s\n", pattern)
	fmt.Printf("objetos na fila simulada: %d\n", len(summary.Outcomes))
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
