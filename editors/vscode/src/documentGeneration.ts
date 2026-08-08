export interface DeliveryToken {
  readonly generation: number;
  readonly revision: number;
}

export class WebviewDeliveryState {
  #generation = 0;
  #ready = false;
  #lastRenderedRevision = -1;
  #inFlightRevision = -1;

  public beginDocument(): void {
    this.#generation += 1;
    this.#ready = false;
    this.#lastRenderedRevision = -1;
    this.#inFlightRevision = -1;
  }

  public markReady(): void {
    this.#generation += 1;
    this.#ready = true;
    this.#lastRenderedRevision = -1;
    this.#inFlightRevision = -1;
  }

  public markHidden(): void {
    this.#generation += 1;
    this.#ready = false;
    this.#inFlightRevision = -1;
  }

  public beginDelivery(revision: number): DeliveryToken | undefined {
    if (
      !this.#ready ||
      this.#inFlightRevision >= 0 ||
      this.#lastRenderedRevision === revision
    ) {
      return undefined;
    }
    this.#inFlightRevision = revision;
    return { generation: this.#generation, revision };
  }

  public rejectDelivery(token: DeliveryToken): boolean {
    if (!this.#matches(token)) {
      return false;
    }
    this.#inFlightRevision = -1;
    this.#ready = false;
    return true;
  }

  public acknowledge(token: DeliveryToken): boolean {
    if (!this.#matches(token)) {
      return false;
    }
    this.#lastRenderedRevision = token.revision;
    this.#inFlightRevision = -1;
    return true;
  }

  public captureGeneration(): number {
    return this.#generation;
  }

  public isCurrent(generation: number): boolean {
    return generation === this.#generation;
  }

  public get ready(): boolean {
    return this.#ready;
  }

  public get lastRenderedRevision(): number {
    return this.#lastRenderedRevision;
  }

  #matches(token: DeliveryToken): boolean {
    return (
      token.generation === this.#generation &&
      token.revision === this.#inFlightRevision
    );
  }
}
