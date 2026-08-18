package stcp

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
)

// Mode é o parâmetro `-m` (§6, p.14). Os três valores são do manual e não se inventam outros.
type Mode string

const (
	// ModeSend — somente transmissão. É o modo do ciclo de saída, e a escolha de NÃO usar `B`
	// (ambos) é deliberada: transmitir e receber na mesma execução misturaria dois desfechos num
	// envelope só, e o contrato do `status/` publica um por ciclo.
	ModeSend Mode = "S"
	// ModeReceive — somente recepção.
	ModeReceive Mode = "R"
	// ModeBoth — transmissão e recepção. Existe no manual; o agente não o usa, pelo motivo acima.
	ModeBoth Mode = "B"
)

// Client aciona o cliente OFTP. É interface para que o ciclo seja testável sem processo externo —
// e, sobretudo, para que o teste de idempotência possa AFIRMAR que o cliente não foi acionado, que
// é o critério de aceite de maior risco deste componente.
type Client interface {
	// Run executa uma passada. O `*int` é o código de saída do processo, e é ponteiro porque a
	// ausência é informação: nil significa que o processo não chegou a ser executado ou que o
	// código não pôde ser observado. O manual NÃO documenta o significado dos códigos de saída
	// (só o resultado no log, §12), então quem consome isto trata como diagnóstico, nunca como
	// veredito.
	Run(ctx context.Context, mode Mode, fileFilter string) (*int, error)
}

// CommandConfig descreve a instalação do cliente na máquina.
//
// Tudo aqui é configuração de ambiente e entra por variável de ambiente na instância: caminho de
// instalação, nome de perfil e OdetteID identificam o convênio, e este repositório é público.
// Nenhum valor default aponta para instalação real.
type CommandConfig struct {
	// ExecutablePath é o caminho completo do executável do cliente.
	ExecutablePath string
	// ConfigPath é o arquivo de configuração da instalação, primeiro argumento posicional (§6).
	ConfigPath string
	// Profile é o parâmetro `-p`.
	Profile string
	// Retries é o parâmetro `-r` — quantidade de tentativas de conexão.
	Retries int
	// RetryIntervalSeconds é o parâmetro `-t` — intervalo entre tentativas.
	RetryIntervalSeconds int
}

// Validate recusa configuração incompleta no boot, em vez de descobrir na primeira transmissão.
func (c CommandConfig) Validate() error {
	switch {
	case c.ExecutablePath == "":
		return errors.New("caminho do executável do cliente STCP não configurado")
	case c.ConfigPath == "":
		return errors.New("arquivo de configuração do cliente STCP não configurado")
	case c.Profile == "":
		return errors.New("perfil do cliente STCP não configurado")
	case c.Retries < 0:
		return fmt.Errorf("tentativas de conexão negativas: %d", c.Retries)
	case c.RetryIntervalSeconds < 0:
		return fmt.Errorf("intervalo entre tentativas negativo: %d", c.RetryIntervalSeconds)
	}
	return nil
}

// CommandClient aciona o executável real.
type CommandClient struct {
	cfg CommandConfig
}

// NewCommandClient devolve o cliente real, recusando configuração inválida.
func NewCommandClient(cfg CommandConfig) (*CommandClient, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &CommandClient{cfg: cfg}, nil
}

// Args monta a linha de comando na ordem do manual (§6, p.14):
//
//	<executável> <arquivo de configuração> -p <perfil> -r <tentativas> -t <intervalo> -m <modo> -f <filtro> -w 0
//
// Exportada porque é o que um teste consegue afirmar sem executar nada — e porque a ordem dos
// argumentos é contrato com um binário de terceiro, não estilo.
//
// `-w 0` fecha a caixa de diálogo ao final. Sem ele, uma execução automatizada deixaria uma janela
// aberta esperando alguém que não existe, e a próxima passada encontraria a máquina travada.
func (c *CommandClient) Args(mode Mode, fileFilter string) []string {
	args := []string{
		c.cfg.ConfigPath,
		"-p", c.cfg.Profile,
		"-r", strconv.Itoa(c.cfg.Retries),
		"-t", strconv.Itoa(c.cfg.RetryIntervalSeconds),
		"-m", string(mode),
	}
	// O filtro é opcional no manual, mas o ciclo de transmissão sempre o informa: é a trava do CA7
	// contra enviar arquivo indevido que esteja na pasta. Um filtro vazio significaria "tudo".
	if fileFilter != "" {
		args = append(args, "-f", fileFilter)
	}
	return append(args, "-w", "0")
}

// Run executa o cliente e devolve o código de saída.
//
// Um código diferente de zero NÃO é erro de execução: significa que o cliente rodou e reportou algo.
// Erro (segundo retorno) fica reservado para o processo não ter conseguido rodar — executável
// ausente, permissão negada, contexto cancelado. A distinção importa porque o ciclo trata os dois
// casos de formas diferentes, e confundi-los faria uma falha de transporte parecer falha de máquina.
func (c *CommandClient) Run(ctx context.Context, mode Mode, fileFilter string) (*int, error) {
	cmd := exec.CommandContext(ctx, c.cfg.ExecutablePath, c.Args(mode, fileFilter)...)

	err := cmd.Run()
	if err == nil {
		zero := 0
		return &zero, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		return &code, nil
	}

	return nil, fmt.Errorf("executar cliente STCP (%s): %w", mode, err)
}
