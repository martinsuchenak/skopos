import Alpine from 'alpinejs';

declare global {
  interface Window {
    Alpine: typeof Alpine;
    app: () => object;
  }
}

window.app = () => ({
  message: 'Welcome to skopos',
  init() {
    console.log('skopos frontend initialized');
  },
});

window.Alpine = Alpine;
Alpine.start();
