package agent

// A publicação do envelope, e a retomada do que não se confirmou.
//
// Este passo é comum aos dois ciclos e era o único ponto em que uma falha ficava sem retomada: o
// erro ia para o desfecho e o passo seguinte rodava assim mesmo — a recepção tirava o arquivo da
// pasta de ENTRADA, a transmissão movia o objeto para fora da fila. Como o registro já dizia
// `done`, nada voltava a passar por ali, e o objeto ficava no bucket sem desfecho publicado.
//
// Do lado de quem consome, o resultado é pior do que parece: um objeto sem envelope é
// indistinguível de um objeto que nunca deveria ter entrado. As duas observações são idênticas e
// pedem ações opostas.
//
// A ordem aqui é a mesma disciplina que o ledger já aplica à intenção de transmissão:
//
//	1. registrar a pendência   (durável, ANTES da tentativa)
//	2. publicar                (a escrita que pode falhar)
//	3. limpar a pendência      (só depois da confirmação)
//
// Registrar DEPOIS de falhar seria inútil: uma queda entre o `Put` que falhou e o registro deixaria
// a pendência sem rastro, que é exatamente o buraco que se está fechando.

import (
	"context"
	"errors"
	"fmt"
)

// publishEnvelope publica o corpo na chave, deixando rastro durável enquanto a publicação não se
// confirma.
//
// Devolve o erro em vez de acumulá-lo: quem chama sabe a que desfecho ele pertence, e os dois
// ciclos anexam o erro de formas diferentes.
func (a *Agent) publishEnvelope(ctx context.Context, key, fileName string, body []byte) error {
	// Sem registro de pendências configurado, publica direto — é o comportamento anterior, e serve
	// a quem monta o agente para um caminho que não precisa de retomada. Produção sempre configura.
	if a.pending == nil {
		if err := a.store.Put(ctx, key, body); err != nil {
			return fmt.Errorf("publicar status em %q: %w", key, err)
		}
		return nil
	}

	if err := a.pending.Save(key, fileName, string(body), a.cfg.Clock()); err != nil {
		// Falhar aqui NÃO impede a publicação: o registro é rede de segurança, e recusar-se a
		// publicar por causa dele trocaria uma falha possível por uma falha certa. O que se perde é
		// a retomada, e o erro sobe junto para que isso apareça.
		if err := a.store.Put(ctx, key, body); err != nil {
			return fmt.Errorf("publicar status em %q: %w", key, err)
		}
		return fmt.Errorf("publicar status em %q funcionou, mas a pendência não pôde ser registrada "+
			"antes (uma falha futura nesta chave não seria retomada): %w", key, err)
	}

	if err := a.store.Put(ctx, key, body); err != nil {
		// A pendência fica gravada de propósito: é ela que faz o ciclo seguinte republicar.
		return fmt.Errorf("publicar status em %q (registrado para nova tentativa no próximo ciclo): %w", key, err)
	}

	if err := a.pending.Clear(key); err != nil {
		// Publicou; só o rastro não saiu. O ciclo seguinte vai republicar o mesmo corpo na mesma
		// chave, o que é inócuo — a escrita é idempotente e o conteúdo é idêntico.
		return fmt.Errorf("status em %q foi publicado, mas a pendência não pôde ser limpa "+
			"(será republicado no próximo ciclo, sem efeito): %w", key, err)
	}
	return nil
}

// ReconcilePending republica os envelopes cuja publicação não se confirmou em ciclos anteriores.
//
// Roda no INÍCIO dos dois ciclos, antes de acionar o cliente: um desfecho que já aconteceu e não
// foi publicado tem precedência sobre qualquer trabalho novo — enquanto ele não sai, existe um
// objeto no bucket que ninguém consegue interpretar.
//
// O corpo republicado é o ORIGINAL, byte a byte. O desfecho não mudou: o arquivo foi recebido e
// depositado, ou a remessa foi transmitida; o que falhou foi a publicação. Reconstruir um envelope
// novo afirmaria outra coisa — e nem seria possível, porque o log daquele ciclo e o relógio daquele
// momento não existem mais.
//
// Erros NÃO abortam o ciclo: eles se acumulam e voltam juntos. Uma pendência que continua falhando
// permanece registrada e reaparece na passada seguinte — desistir em silêncio recriaria o órfão
// com um passo a mais.
func (a *Agent) ReconcilePending(ctx context.Context) (int, error) {
	if a.pending == nil {
		return 0, nil
	}

	pendentes, err := a.pending.List()
	if err != nil {
		return 0, fmt.Errorf("listar envelopes pendentes: %w", err)
	}

	var (
		republicados int
		errs         []error
	)
	for _, p := range pendentes {
		if err := a.store.Put(ctx, p.Chave, []byte(p.Corpo)); err != nil {
			errs = append(errs, fmt.Errorf("republicar envelope pendente de %q em %q "+
				"(registrado desde %s; o objeto segue sem desfecho publicado): %w",
				p.Arquivo, p.Chave, p.RegistradoEm, err))
			continue
		}
		if err := a.pending.Clear(p.Chave); err != nil {
			errs = append(errs, err)
			continue
		}
		republicados++
	}
	return republicados, errors.Join(errs...)
}
