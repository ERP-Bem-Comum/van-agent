// Binário que ENCENA o cliente STCP OFTP do banco, para exercitar o agente sem tocar a VAN.
//
// ⚠️⚠️ ESTE PROGRAMA NÃO TRANSMITE NADA. ⚠️⚠️
//
// Ele existe porque o `stcpfake` — o duplo fiel ao manual — é um pacote Go, injetado como
// `stcp.Client` na suíte. Fora da suíte o agente aciona um EXECUTÁVEL (`stcp.NewCommandClient`), e
// quem quisesse montar uma simulação de ponta a ponta precisava escrever o seu próprio programa. Foi
// o que aconteceu: o core-api montou um de 15 linhas, e passamos a ter dois modelos do mesmo
// sistema. Dois simuladores diferentes significam testar sistemas diferentes, e o teto de confiança
// da frente fica mais baixo do que precisa.
//
// Este binário é o mesmo `stcpfake` da suíte, com uma linha de comando por fora. O que a suíte prova
// e o que a simulação prova passam a ser a mesma coisa.
//
// # O perigo deste programa, e a trava que ele carrega
//
// Um falso cliente é perigoso justamente porque PARECE funcionar: apontar o agente para cá numa
// instalação real produziria envelopes de "transmitido" sobre pagamentos que nunca saíram, e nada no
// desfecho denunciaria. Por isso ele recusa rodar sem `STCP_ENCENADO_CONFIRMO` (ver abaixo) e
// anuncia o que é em toda execução. A confirmação é chata de propósito: ela não pode ser algo que
// alguém digite por reflexo.
//
// # Como o agente o invoca
//
// A linha de comando é a do manual (§6, p.14), montada por `stcp.CommandClient.Args`:
//
//	<executável> <arquivo de configuração> -p <perfil> -r <n> -t <n> -m <S|R|B> [-f <filtro>] -w 0
//
// O primeiro argumento é POSICIONAL, e é por isso que o parse aqui é manual: `flag` do Go para de
// interpretar na primeira coisa que não é flag, e devolveria o resto como argumentos soltos.
//
// O `-f` é honrado, e não é detalhe: um duplo que ignorasse o filtro esconderia exatamente o defeito
// que a trava de nomenclatura existe para barrar.
//
// # O que ele lê do ambiente
//
// As pastas NÃO vêm da linha de comando — o cliente real as lê do perfil, e aqui elas vêm do
// ambiente, com prefixo próprio para nunca serem confundidas com as do agente:
//
//	STCP_ENCENADO_CONFIRMO        obrigatório, valor exato `nao-transmite-nada`
//	STCP_ENCENADO_OUTBOUND_DIR    pasta de SAÍDA  (obrigatória no modo S e B)
//	STCP_ENCENADO_BACKUP_DIR      pasta de BACKUP (obrigatória no modo S e B)
//	STCP_ENCENADO_LOG_PATH        arquivo do log posicional (§12, p.30)
//	STCP_ENCENADO_INBOUND_DIR     pasta de ENTRADA (obrigatória no modo R e B)
//	STCP_ENCENADO_ENTREGAR_DE     pasta com o que o "banco" vai entregar (modo R e B)
//	STCP_ENCENADO_PERFIL          nome do perfil gravado no log; default PERFIL-DE-TESTE
//	STCP_ENCENADO_COMPORTAMENTO   sucesso | recusa | sumico | falha-de-execucao; default sucesso
//	STCP_ENCENADO_APLICAR_A       regex; o comportamento acima vale só para os nomes que casam
//	STCP_ENCENADO_CODIGO_DE_FALHA resultado gravado na recusa (§11, pp. 24-29); default 000401
//	STCP_ENCENADO_ENTREGAR_SEM_LOG regex; o que casar é entregue SEM linha de log no ciclo
//
// Nenhum default aponta para caminho real, pelo mesmo motivo que no agente.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/ERP-Bem-Comum/van-agent/internal/stcp"
	"github.com/ERP-Bem-Comum/van-agent/internal/stcp/stcpfake"
)

// A confirmação é uma frase, e não um `1` ou um `true`, porque precisa ser impossível de ligar sem
// ler o que ela diz.
const (
	confirmVar   = "STCP_ENCENADO_CONFIRMO"
	confirmValue = "nao-transmite-nada"
)

// Códigos de saída. Espelham os do agente de propósito: quem opera as duas coisas lê a mesma tabela.
const (
	exitOK       = 0
	exitSoftware = 70
	exitConfig   = 78
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run é separado do `main` para que o teste exercite a mesma função que o processo executa —
// um teste que reimplementasse o fluxo verificaria o teste, não o programa.
func run(argv []string, stdout, stderr io.Writer) int {
	if os.Getenv(confirmVar) != confirmValue {
		fmt.Fprintf(stderr, `RECUSADO: este programa ENCENA o cliente STCP e NÃO TRANSMITE NADA.

Apontar o agente para cá numa instalação real produziria envelopes de "transmitido"
sobre pagamentos que nunca saíram, e o desfecho não denunciaria isso.

Para usar em simulação, defina:  %s=%s
`, confirmVar, confirmValue)
		return exitConfig
	}

	invocacao, err := parseArgs(argv)
	if err != nil {
		fmt.Fprintf(stderr, "linha de comando: %v\n", err)
		return exitConfig
	}

	fake, err := montarFake(invocacao.Mode)
	if err != nil {
		fmt.Fprintf(stderr, "ambiente: %v\n", err)
		return exitConfig
	}

	fmt.Fprintf(stdout, "⚠️  ENCENAÇÃO — nada é transmitido · modo=%s · perfil=%s · filtro=%q\n",
		invocacao.Mode, fake.Profile, invocacao.Filter)

	exit, err := fake.Run(context.Background(), invocacao.Mode, invocacao.Filter)
	if err != nil {
		// `falha-de-execucao` chega aqui de propósito: é como o processo que não roda se parece para
		// o agente, e é o caminho que o agente precisa distinguir de "rodou e recusou".
		fmt.Fprintf(stderr, "encenação: %v\n", err)
		return exitSoftware
	}
	if exit == nil {
		return exitOK
	}
	return *exit
}

// Invocacao é o que a linha de comando do §6 carrega e que muda o comportamento.
type Invocacao struct {
	ConfigPath string
	Profile    string
	Mode       stcp.Mode
	Filter     string
}

// parseArgs lê a linha de comando do §6 (p.14).
//
// Manual porque o primeiro argumento é posicional e o `flag` do Go pararia ali. As flags
// desconhecidas são ACEITAS e ignoradas — o manual documenta outras, e recusar o que não conhecemos
// faria a encenação divergir do cliente real justamente na parte que não estamos modelando.
func parseArgs(argv []string) (Invocacao, error) {
	var inv Invocacao

	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if !strings.HasPrefix(a, "-") {
			if inv.ConfigPath == "" {
				inv.ConfigPath = a
			}
			continue
		}

		valor := func() string {
			if i+1 < len(argv) {
				i++
				return argv[i]
			}
			return ""
		}

		switch a {
		case "-p":
			inv.Profile = valor()
		case "-m":
			inv.Mode = stcp.Mode(valor())
		case "-f":
			inv.Filter = valor()
		case "-r", "-t", "-w":
			// Reconhecidos e ignorados: retentativa, intervalo e a caixa de diálogo não mudam a
			// evidência física, que é o que o agente observa.
			valor()
		}
	}

	switch inv.Mode {
	case stcp.ModeSend, stcp.ModeReceive, stcp.ModeBoth:
	case "":
		return inv, errors.New("modo não informado (-m); esperado S, R ou B")
	default:
		return inv, fmt.Errorf("modo desconhecido %q; esperado S, R ou B", inv.Mode)
	}
	return inv, nil
}

// montarFake lê o ambiente e devolve o duplo configurado.
//
// A cobrança de pastas é POR MODO, como no agente: exigir a pasta de entrada de quem só transmite
// faria a simulação falhar por algo que ela não usa.
func montarFake(mode stcp.Mode) (*stcpfake.Fake, error) {
	var faltando []string
	exige := func(nome string) string {
		v := os.Getenv(nome)
		if v == "" {
			faltando = append(faltando, nome)
		}
		return v
	}

	envia := mode == stcp.ModeSend || mode == stcp.ModeBoth
	recebe := mode == stcp.ModeReceive || mode == stcp.ModeBoth

	logPath := exige("STCP_ENCENADO_LOG_PATH")
	var outbound, backup, inbound, entregarDe string
	if envia {
		outbound = exige("STCP_ENCENADO_OUTBOUND_DIR")
		backup = exige("STCP_ENCENADO_BACKUP_DIR")
	}
	if recebe {
		inbound = exige("STCP_ENCENADO_INBOUND_DIR")
		entregarDe = exige("STCP_ENCENADO_ENTREGAR_DE")
	}
	if len(faltando) > 0 {
		return nil, fmt.Errorf("variáveis ausentes para o modo %s: %s", mode, strings.Join(faltando, ", "))
	}

	fake := stcpfake.New(outbound, backup, logPath)
	fake.InboundDir = inbound
	if p := os.Getenv("STCP_ENCENADO_PERFIL"); p != "" {
		fake.Profile = p
	}
	if c := os.Getenv("STCP_ENCENADO_CODIGO_DE_FALHA"); c != "" {
		fake.FailureCode = c
	}

	comportamento, err := lerComportamento()
	if err != nil {
		return nil, err
	}
	fake.Behavior = comportamento

	if recebe {
		entregas, err := lerEntregas(entregarDe)
		if err != nil {
			return nil, err
		}
		fake.Incoming = entregas
	}
	return fake, nil
}

// lerComportamento traduz `STCP_ENCENADO_COMPORTAMENTO` e o escopo dele.
//
// `APLICAR_A` existe para que uma simulação possa encenar UM arquivo problemático no meio de uma fila
// que passa: encenar por fila inteira só exercitaria os extremos, e o que interessa é o arquivo
// ruim ao lado dos bons.
func lerComportamento() (func(string) stcpfake.Behavior, error) {
	bruto := strings.TrimSpace(os.Getenv("STCP_ENCENADO_COMPORTAMENTO"))
	if bruto == "" || bruto == "sucesso" {
		return nil, nil // nil = sucesso para todos, que é o default do duplo
	}

	var escolhido stcpfake.Behavior
	switch bruto {
	case "recusa":
		escolhido = stcpfake.Reject
	case "sumico":
		escolhido = stcpfake.Vanish
	case "falha-de-execucao":
		escolhido = stcpfake.Crash
	default:
		return nil, fmt.Errorf("STCP_ENCENADO_COMPORTAMENTO desconhecido: %q "+
			"(esperado sucesso, recusa, sumico ou falha-de-execucao)", bruto)
	}

	escopo := os.Getenv("STCP_ENCENADO_APLICAR_A")
	if escopo == "" {
		return func(string) stcpfake.Behavior { return escolhido }, nil
	}
	re, err := regexp.Compile(escopo)
	if err != nil {
		return nil, fmt.Errorf("STCP_ENCENADO_APLICAR_A não compila: %w", err)
	}
	return func(nome string) stcpfake.Behavior {
		if re.MatchString(nome) {
			return escolhido
		}
		return stcpfake.Succeed
	}, nil
}

// entreguesDir é para onde vai o que já foi entregue.
const entreguesDir = "entregues"

// lerEntregas lê o que o "banco" tem para entregar e MOVE cada arquivo para `entregues/`.
//
// O movimento é o que torna a encenação fiel: o cliente real não reentrega o que já entregou. Sem
// ele, cada acionamento reentregaria a pasta inteira, e o agente — que arquiva o recebido — veria o
// mesmo conteúdo voltar para sempre, escondendo a diferença entre "o banco reenviou" e "ninguém
// tirou da pasta".
func lerEntregas(dir string) ([]stcpfake.Incoming, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Pasta ausente é "nada a entregar", não erro: uma simulação que só transmite não precisa
			// criá-la, e o ciclo de recepção sem nada a receber é desfecho legítimo.
			return nil, nil
		}
		return nil, fmt.Errorf("ler pasta de entregas %q: %w", dir, err)
	}

	semLog, err := compilarSemLog()
	if err != nil {
		return nil, err
	}

	destino := filepath.Join(dir, entreguesDir)
	nomes := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		nomes = append(nomes, e.Name())
	}
	// Ordem estável: sem ela, dois arquivos entregues no mesmo ciclo apareceriam no log em ordem de
	// sistema de arquivos, e uma investigação sobre qual chegou primeiro não se reproduziria.
	sort.Strings(nomes)

	out := make([]stcpfake.Incoming, 0, len(nomes))
	for _, nome := range nomes {
		conteudo, err := os.ReadFile(filepath.Join(dir, nome))
		if err != nil {
			return nil, fmt.Errorf("ler entrega %q: %w", nome, err)
		}
		out = append(out, stcpfake.Incoming{
			Name:    nome,
			Content: conteudo,
			Logged:  semLog == nil || !semLog.MatchString(nome),
		})
	}

	if len(out) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(destino, 0o750); err != nil {
		return nil, fmt.Errorf("criar pasta de entregues: %w", err)
	}
	for _, in := range out {
		if err := os.Rename(filepath.Join(dir, in.Name), filepath.Join(destino, in.Name)); err != nil {
			return nil, fmt.Errorf("mover entrega %q para %q: %w", in.Name, entreguesDir, err)
		}
	}
	return out, nil
}

func compilarSemLog() (*regexp.Regexp, error) {
	bruto := os.Getenv("STCP_ENCENADO_ENTREGAR_SEM_LOG")
	if bruto == "" {
		return nil, nil
	}
	re, err := regexp.Compile(bruto)
	if err != nil {
		return nil, fmt.Errorf("STCP_ENCENADO_ENTREGAR_SEM_LOG não compila: %w", err)
	}
	return re, nil
}
